// Package upstream watches the JP masterdata source for new game data and
// triggers a CN sync when it changes. Instead of polling the GitHub commits API
// (which is rate-limited — see the project notes), it fetches the raw
// versions/current_version.json and compares dataVersion. Optionally it keeps a
// local git clone of the masterdata repo refreshed (git pull) so future work can
// read masterdata from disk without hammering the API.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/httpx"
)

const (
	defaultPollInterval       = time.Hour
	defaultRateLimitCooldown  = time.Hour
	maxRetryAfterCooldown     = 24 * time.Hour
	maxFallbackCooldown       = 6 * time.Hour
	defaultVersionURL         = "https://metadata.pjsk.moe/jp/versions/current_version.json"
	defaultVersionFallbackURL = "https://raw.githubusercontent.com/{repo}/{branch}/versions/current_version.json"
)

var builtInVersionFallbackURLs = []string{
	"https://raw.githubusercontent.com/{repo}/{branch}/versions/current_version.json",
	"https://fastly.jsdelivr.net/gh/{repo}@{branch}/versions/current_version.json",
	"https://gcore.jsdelivr.net/gh/{repo}@{branch}/versions/current_version.json",
	"https://cdn.jsdelivr.net/gh/{repo}@{branch}/versions/current_version.json",
}

// VersionInfo is the subset of current_version.json we care about.
type VersionInfo struct {
	AppVersion   string `json:"appVersion"`
	DataVersion  string `json:"dataVersion"`
	AssetVersion string `json:"assetVersion"`
}

// SyncFn runs a CN sync. Returning an error keeps the change pending for retry.
type SyncFn func() error

// Status reports the watcher's state for the admin UI.
type Status struct {
	Enabled             bool     `json:"enabled"`
	Repo                string   `json:"repo"`
	Branch              string   `json:"branch"`
	VersionURL          string   `json:"versionURL,omitempty"`
	VersionFallbackURL  string   `json:"versionFallbackURL,omitempty"`
	VersionFallbackURLs []string `json:"versionFallbackURLs,omitempty"`
	LastSource          string   `json:"lastSource,omitempty"`
	LastCheck           string   `json:"lastCheck,omitempty"`
	LastSuccess         string   `json:"lastSuccess,omitempty"`
	LastDataVersion     string   `json:"lastDataVersion,omitempty"`
	ChangeDetectedAt    string   `json:"changeDetectedAt,omitempty"`
	LastSync            string   `json:"lastSync,omitempty"`
	LastError           string   `json:"lastError,omitempty"`
	LastErrorAt         string   `json:"lastErrorAt,omitempty"`
	ConsecutiveFailures int      `json:"consecutiveFailures,omitempty"`
	RateLimitedUntil    string   `json:"rateLimitedUntil,omitempty"`
	GitMirrorReady      bool     `json:"gitMirrorReady"`
}

// Watcher polls current_version.json and triggers sync on dataVersion change.
type Watcher struct {
	cfg      *config.Config
	syncFn   SyncFn
	client   *http.Client
	interval time.Duration
	gitDir   string // local clone path; empty disables the git mirror
	useGit   bool

	checkMu          sync.Mutex
	mu               sync.Mutex
	status           Status
	etag             string
	lastModified     string
	validatorURL     string
	cachedVersion    VersionInfo
	rateLimitedUntil time.Time

	stopCh chan struct{}
}

// Options configures the watcher.
type Options struct {
	Interval time.Duration // poll interval (default 1h)
	GitDir   string        // local masterdata clone dir; "" disables git mirror
	UseGit   bool          // whether to maintain the git mirror
}

func New(cfg *config.Config, syncFn SyncFn, opts Options) *Watcher {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	return &Watcher{
		cfg:      cfg,
		syncFn:   syncFn,
		client:   httpx.NewClient(30 * time.Second),
		interval: interval,
		gitDir:   opts.GitDir,
		useGit:   opts.UseGit && opts.GitDir != "",
		stopCh:   make(chan struct{}),
	}
}

// versionURL builds the current_version.json URL for the configured repo.
func (w *Watcher) versionURL() string {
	repo := w.cfg.GetOr(config.KeyUpstreamRepo, "Team-Haruki/haruki-sekai-master")
	branch := w.cfg.GetOr(config.KeyUpstreamBranch, "main")
	return expandVersionURL(w.cfg.Get(config.KeyUpstreamVersionURL), repo, branch)
}

func (w *Watcher) versionURLs() []string {
	repo := w.cfg.GetOr(config.KeyUpstreamRepo, "Team-Haruki/haruki-sekai-master")
	branch := w.cfg.GetOr(config.KeyUpstreamBranch, "main")
	primary := expandVersionURL(w.cfg.Get(config.KeyUpstreamVersionURL), repo, branch)
	fallbackSetting := w.cfg.GetOr(config.KeyUpstreamVersionFallbackURL, defaultVersionFallbackURL)
	templates := append(splitVersionTemplates(fallbackSetting), builtInVersionFallbackURLs...)
	urls := []string{primary}
	seen := map[string]bool{primary: true}
	for _, tmpl := range templates {
		url := expandVersionTemplate(tmpl, repo, branch)
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		urls = append(urls, url)
	}
	return urls
}

func splitVersionTemplates(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	})
}

func expandVersionURL(tmpl, repo, branch string) string {
	tmpl = strings.TrimSpace(tmpl)
	if tmpl == "" {
		tmpl = defaultVersionURL
	}
	return expandVersionTemplate(tmpl, repo, branch)
}

func expandVersionTemplate(tmpl, repo, branch string) string {
	tmpl = strings.TrimSpace(tmpl)
	tmpl = strings.ReplaceAll(tmpl, "{repo}", repo)
	tmpl = strings.ReplaceAll(tmpl, "{branch}", branch)
	return tmpl
}

// Start launches the polling loop unless disabled in config.
func (w *Watcher) Start() {
	if !w.cfg.GetBool(config.KeySchedulerOn, true) {
		fmt.Println("[upstream] scheduler disabled by config")
		w.setStatus(func(s *Status) { s.Enabled = false })
		return
	}
	w.setStatus(func(s *Status) {
		s.Enabled = true
		s.Repo = w.cfg.GetOr(config.KeyUpstreamRepo, "Team-Haruki/haruki-sekai-master")
		s.Branch = w.cfg.GetOr(config.KeyUpstreamBranch, "main")
		w.updateStatusSources(s)
	})
	if w.useGit {
		go w.ensureGitMirror()
	}
	go w.loop()
}

func (w *Watcher) Stop() { close(w.stopCh) }

func (w *Watcher) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *Watcher) setStatus(fn func(*Status)) {
	w.mu.Lock()
	fn(&w.status)
	w.mu.Unlock()
}

func (w *Watcher) updateStatusSources(s *Status) {
	urls := w.versionURLs()
	s.VersionURL = ""
	s.VersionFallbackURL = ""
	s.VersionFallbackURLs = nil
	if len(urls) == 0 {
		return
	}
	s.VersionURL = urls[0]
	if len(urls) > 1 {
		s.VersionFallbackURL = urls[1]
		s.VersionFallbackURLs = append([]string(nil), urls[1:]...)
	}
}

func (w *Watcher) loop() {
	// Record the initial version without triggering a sync (avoids a sync on
	// every restart). A change relative to this baseline triggers work.
	w.check(true)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.check(false)
		}
	}
}

// CheckNow runs an immediate check (admin "check now" button). When force is
// true it triggers a sync even if the version is unchanged.
func (w *Watcher) CheckNow(force bool) (Status, error) {
	changed, err := w.fetchAndCompare()
	if err != nil {
		return w.Status(), err
	}
	if changed || force {
		if err := w.runSync(); err != nil {
			return w.Status(), err
		}
	}
	return w.Status(), nil
}

func (w *Watcher) check(baseline bool) {
	changed, err := w.fetchAndCompare()
	if err != nil {
		fmt.Printf("[upstream] check failed: %v\n", err)
		return
	}
	if baseline {
		fmt.Printf("[upstream] baseline dataVersion=%s\n", w.Status().LastDataVersion)
		return
	}
	if changed {
		if err := w.runSync(); err != nil {
			fmt.Printf("[upstream] sync failed: %v\n", err)
		}
	}
}

// fetchAndCompare fetches the version file and updates status, returning whether
// dataVersion changed since the last observed value.
func (w *Watcher) fetchAndCompare() (bool, error) {
	w.checkMu.Lock()
	defer w.checkMu.Unlock()

	info, sourceURL, err := w.fetchVersion()
	checkedAt := nowRFC3339()
	if err != nil {
		w.setStatus(func(s *Status) {
			s.LastCheck = checkedAt
			s.LastError = err.Error()
			s.LastErrorAt = checkedAt
			s.ConsecutiveFailures++
			w.updateStatusSources(s)
		})
		return false, err
	}
	changed := false
	w.setStatus(func(s *Status) {
		s.LastCheck = checkedAt
		s.LastSuccess = checkedAt
		s.LastError = ""
		s.LastErrorAt = ""
		s.ConsecutiveFailures = 0
		w.updateStatusSources(s)
		s.LastSource = sourceURL
		if s.LastDataVersion != "" && s.LastDataVersion != info.DataVersion {
			changed = true
			s.ChangeDetectedAt = checkedAt
		}
		s.LastDataVersion = info.DataVersion
	})
	if changed {
		fmt.Printf("[upstream] dataVersion changed -> %s\n", info.DataVersion)
	}
	return changed, nil
}

func (w *Watcher) fetchVersion() (VersionInfo, string, error) {
	var info VersionInfo
	now := time.Now()
	_, _, _, rateLimitedUntil := w.fetchState("")
	if !rateLimitedUntil.IsZero() {
		if now.Before(rateLimitedUntil) {
			return info, "", fmt.Errorf("version fetch paused after HTTP 429; retry after %s", rateLimitedUntil.UTC().Format(time.RFC3339))
		}
		w.clearRateLimit()
	}

	type failedSource struct {
		sourceURL  string
		err        error
		retryAfter string
	}
	type fetchResult struct {
		info       VersionInfo
		sourceURL  string
		retryAfter string
		err        error
	}
	runRound := func(urls []string) (VersionInfo, string, []failedSource, bool) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		results := make(chan fetchResult, len(urls))
		for _, sourceURL := range urls {
			go func() {
				fetched, retryAfter, err := w.fetchVersionURL(ctx, sourceURL)
				results <- fetchResult{info: fetched, sourceURL: sourceURL, retryAfter: retryAfter, err: err}
			}()
		}
		failures := make([]failedSource, 0, len(urls))
		for range urls {
			result := <-results
			if result.err == nil {
				cancel()
				return result.info, result.sourceURL, failures, true
			}
			failures = append(failures, failedSource{
				sourceURL: result.sourceURL,
				err:       result.err, retryAfter: result.retryAfter,
			})
		}
		return VersionInfo{}, "", failures, false
	}

	urls := w.versionURLs()
	fetched, sourceURL, failures, ok := runRound(urls)
	if ok {
		return fetched, sourceURL, nil
	}

	var retryable []string
	for _, failure := range failures {
		if isRetryableVersionError(failure.err) {
			retryable = append(retryable, failure.sourceURL)
		}
	}
	if len(retryable) > 0 {
		time.Sleep(500 * time.Millisecond)
		fetched, sourceURL, retryFailures, ok := runRound(retryable)
		if ok {
			return fetched, sourceURL, nil
		}
		for _, failure := range retryFailures {
			failure.err = fmt.Errorf("retry: %w", failure.err)
			failures = append(failures, failure)
		}
	}

	for _, failure := range failures {
		if failure.retryAfter == "" && !strings.Contains(failure.err.Error(), "http 429") {
			continue
		}
		until := now.Add(rateLimitCooldown(failure.retryAfter, now, w.interval))
		w.setRateLimit(until)
		break
	}
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, failure.err.Error())
	}
	return info, "", fmt.Errorf("all version sources failed: %s", strings.Join(parts, "; "))
}

func (w *Watcher) fetchVersionURL(ctx context.Context, sourceURL string) (VersionInfo, string, error) {
	var info VersionInfo
	etag, lastModified, cached, _ := w.fetchState(sourceURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return info, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "moesekai-upstream-watcher")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return info, "", fmt.Errorf("GET %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		if cached.DataVersion == "" {
			return info, "", fmt.Errorf("GET %s: http 304 but no cached dataVersion", sourceURL)
		}
		w.updateValidators(resp, cached, sourceURL)
		return cached, "", nil
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			return info, resp.Header.Get("Retry-After"), fmt.Errorf("GET %s: http 429: version source rate limited", sourceURL)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return info, "", fmt.Errorf("GET %s: http %d: %s", sourceURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return info, "", fmt.Errorf("GET %s: decode: %w", sourceURL, err)
	}
	if info.DataVersion == "" {
		return info, "", fmt.Errorf("GET %s: current_version.json missing dataVersion", sourceURL)
	}
	w.updateValidators(resp, info, sourceURL)
	return info, "", nil
}

func (w *Watcher) fetchState(sourceURL string) (etag, lastModified string, cached VersionInfo, rateLimitedUntil time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if sourceURL == "" || sourceURL == w.validatorURL {
		etag, lastModified = w.etag, w.lastModified
	}
	return etag, lastModified, w.cachedVersion, w.rateLimitedUntil
}

func (w *Watcher) updateValidators(resp *http.Response, info VersionInfo, sourceURL string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cachedVersion = info
	w.validatorURL = sourceURL
	if etag := resp.Header.Get("ETag"); etag != "" || resp.StatusCode == http.StatusOK {
		w.etag = etag
	}
	if lastModified := resp.Header.Get("Last-Modified"); lastModified != "" || resp.StatusCode == http.StatusOK {
		w.lastModified = lastModified
	}
	w.rateLimitedUntil = time.Time{}
	w.status.RateLimitedUntil = ""
}

func isRetryableVersionError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "http 500") ||
		strings.Contains(s, "http 502") ||
		strings.Contains(s, "http 503") ||
		strings.Contains(s, "http 504") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF")
}

func (w *Watcher) setRateLimit(until time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rateLimitedUntil = until
	w.status.RateLimitedUntil = until.UTC().Format(time.RFC3339)
}

func (w *Watcher) clearRateLimit() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rateLimitedUntil = time.Time{}
	w.status.RateLimitedUntil = ""
}

func rateLimitCooldown(retryAfter string, now time.Time, interval time.Duration) time.Duration {
	if d := parseRetryAfter(retryAfter, now); d > 0 {
		if d > maxRetryAfterCooldown {
			return maxRetryAfterCooldown
		}
		return d
	}
	d := interval * 2
	if d < defaultRateLimitCooldown {
		d = defaultRateLimitCooldown
	}
	if d > maxFallbackCooldown {
		d = maxFallbackCooldown
	}
	return d
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	t, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	if d := t.Sub(now); d > 0 {
		return d
	}
	return 0
}

// RecordSyncResult updates the shared upstream status after a scheduled or
// manual data sync. A complete manual sync clears a stale watcher error because
// it has just verified the configured upstream data path end to end.
func (w *Watcher) RecordSyncResult(err error) {
	stamp := nowRFC3339()
	w.setStatus(func(s *Status) {
		if err != nil {
			s.LastError = err.Error()
			s.LastErrorAt = stamp
			return
		}
		s.LastSync = stamp
		s.LastSuccess = stamp
		s.LastError = ""
		s.LastErrorAt = ""
		s.ConsecutiveFailures = 0
	})
}

// runSync refreshes the git mirror (if enabled) then runs the CN sync.
func (w *Watcher) runSync() error {
	if w.useGit {
		if err := w.pullGitMirror(); err != nil {
			fmt.Printf("[upstream] git mirror pull failed (continuing with HTTP sync): %v\n", err)
		}
	}
	if w.syncFn == nil {
		return nil
	}
	if err := w.syncFn(); err != nil {
		w.RecordSyncResult(err)
		return err
	}
	w.RecordSyncResult(nil)
	fmt.Println("[upstream] sync completed after upstream change")
	return nil
}

// ---- git mirror (optional) ----

func (w *Watcher) repoURL() string {
	repo := w.cfg.GetOr(config.KeyUpstreamRepo, "Team-Haruki/haruki-sekai-master")
	return fmt.Sprintf("https://github.com/%s.git", repo)
}

// ensureGitMirror clones the masterdata repo on first run (shallow), or marks
// the mirror ready if it already exists.
func (w *Watcher) ensureGitMirror() {
	if w.gitDir == "" {
		return
	}
	gitPath := filepath.Join(w.gitDir, ".git")
	if _, err := os.Stat(gitPath); err == nil {
		w.setStatus(func(s *Status) { s.GitMirrorReady = true })
		return
	}
	if err := os.MkdirAll(filepath.Dir(w.gitDir), 0o755); err != nil {
		fmt.Printf("[upstream] git mirror mkdir failed: %v\n", err)
		return
	}
	branch := w.cfg.GetOr(config.KeyUpstreamBranch, "main")
	fmt.Printf("[upstream] cloning masterdata mirror (shallow) -> %s\n", w.gitDir)
	if err := runGit(w.gitDir, true, "clone", "--depth", "1", "--branch", branch, w.repoURL(), w.gitDir); err != nil {
		fmt.Printf("[upstream] git clone failed: %v\n", err)
		return
	}
	w.setStatus(func(s *Status) { s.GitMirrorReady = true })
	fmt.Println("[upstream] git mirror ready")
}

// pullGitMirror fast-forwards the local mirror. Clones first if absent.
func (w *Watcher) pullGitMirror() error {
	if w.gitDir == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(w.gitDir, ".git")); err != nil {
		w.ensureGitMirror()
		return nil
	}
	branch := w.cfg.GetOr(config.KeyUpstreamBranch, "main")
	return runGit(w.gitDir, false, "pull", "--ff-only", "origin", branch)
}

// runGit runs a git command. When isClone is true, dir is the parent (the clone
// target is in args); otherwise dir is the repo working directory.
func runGit(dir string, isClone bool, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if isClone {
		cmd.Dir = filepath.Dir(dir)
	} else {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
