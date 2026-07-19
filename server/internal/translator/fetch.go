// Package translator ports the legacy CN-sync + AI translation engine to the
// SQLite-backed store. It fetches masterdata from JP/CN mirrors, extracts
// translatable text per category, applies official CN translations, and fills
// gaps with LLM translation (Gemini / OpenAI-compatible).
package translator

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"moesekai/server/internal/config"
)

const (
	defaultJPMasterdataURL         = "https://metadata.pjsk.moe/jp/master"
	defaultJPMasterdataFallbackURL = "https://raw.githubusercontent.com/{repo}/{branch}/master"
	defaultCNMasterdataURL         = "https://metadata.pjsk.moe/cn/master"
	defaultCNMasterdataFallbackURL = "https://sekaimaster-cn.exmeaning.com/master"

	// snowyassets.exmeaning.com currently returns HTTP 525 for scenario files,
	// so the previously secondary mirror is now the healthy primary.
	defaultJPAssetsURL         = "https://assets.unipjsk.com/ondemand"
	defaultJPAssetsFallbackURL = ""
	defaultCNAssetsURL         = "https://sekai-assets-bdf29c81.seiunx.net/cn-assets/ondemand"
	defaultCNAssetsFallbackURL = ""
	defaultSourceHedgeDelay    = 2 * time.Second
)

type sourceFailure struct {
	url string
	err error
}

// fetchMasterdata fetches a masterdata array from the JP or CN source chain.
func (t *Translator) fetchMasterdata(filename, server string) ([]map[string]any, error) {
	data, err := t.fetchMasterdataDocument(filename, server)
	if err != nil {
		return nil, err
	}
	arr, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected json type for %s", filename)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (t *Translator) fetchMasterdataDocument(filename, server string) (any, error) {
	return t.fetchJSONFromBases(t.masterdataBases(server), filename)
}

func (t *Translator) masterdataBases(server string) []string {
	if server == "cn" {
		return t.sourceURLs(
			config.KeyUpstreamCNMasterdataURL, defaultCNMasterdataURL,
			config.KeyUpstreamCNMasterdataFallbackURL, defaultCNMasterdataFallbackURL,
		)
	}
	return t.sourceURLs(
		config.KeyUpstreamJPMasterdataURL, defaultJPMasterdataURL,
		config.KeyUpstreamJPMasterdataFallbackURL, defaultJPMasterdataFallbackURL,
	)
}

func (t *Translator) jpAssetBases() []string {
	return t.sourceURLs(
		config.KeyUpstreamJPAssetsURL, defaultJPAssetsURL,
		config.KeyUpstreamJPAssetsFallbackURL, defaultJPAssetsFallbackURL,
	)
}

func (t *Translator) cnAssetBases() []string {
	return t.sourceURLs(
		config.KeyUpstreamCNAssetsURL, defaultCNAssetsURL,
		config.KeyUpstreamCNAssetsFallbackURL, defaultCNAssetsFallbackURL,
	)
}

func (t *Translator) sourceURLs(primaryKey, primaryDefault, fallbackKey, fallbackDefault string) []string {
	primary := primaryDefault
	fallback := fallbackDefault
	if t.cfg != nil {
		primary = t.cfg.GetOr(primaryKey, primaryDefault)
		fallback = t.cfg.GetOr(fallbackKey, fallbackDefault)
	}
	return dedupeURLs([]string{t.expandSourceTemplate(primary), t.expandSourceTemplate(fallback)})
}

func (t *Translator) expandSourceTemplate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	repo, branch := "Team-Haruki/haruki-sekai-master", "main"
	if t.cfg != nil {
		repo = t.cfg.GetOr(config.KeyUpstreamRepo, repo)
		branch = t.cfg.GetOr(config.KeyUpstreamBranch, branch)
	}
	value = strings.ReplaceAll(value, "{repo}", repo)
	value = strings.ReplaceAll(value, "{branch}", branch)
	return strings.TrimRight(value, "/")
}

func (t *Translator) fetchConcurrency() int {
	n := 4
	if t.cfg != nil {
		n = t.cfg.GetInt(config.KeyUpstreamFetchConcurrency, n)
	}
	if n < 1 {
		return 1
	}
	if n > 12 {
		return 12
	}
	return n
}

func (t *Translator) fetchHedgeDelay() time.Duration {
	if t.hedgeDelay > 0 {
		return t.hedgeDelay
	}
	return defaultSourceHedgeDelay
}

// fetchJSONURL fetches and decodes JSON. Transient failures are retried once;
// source-chain callers try every configured mirror before retrying a failed
// source, so a dead primary cannot block a healthy fallback for minutes.
func (t *Translator) fetchJSONURL(url string) (any, error) {
	return t.fetchJSONURLs([]string{url})
}

func (t *Translator) fetchJSONFromBases(bases []string, path string) (any, error) {
	urls := make([]string, 0, len(bases))
	for _, base := range bases {
		urls = append(urls, joinSourceURL(base, path))
	}
	return t.fetchJSONURLs(urls)
}

func (t *Translator) fetchJSONURLs(urls []string) (any, error) {
	urls = dedupeURLs(urls)
	if len(urls) == 0 {
		return nil, fmt.Errorf("no upstream source configured")
	}

	result, failures, ok := t.fetchJSONRound(urls)
	if ok {
		return result, nil
	}

	retryable := make([]string, 0, len(failures))
	for _, failure := range failures {
		if isTransientErr(failure.err) {
			retryable = append(retryable, failure.url)
		}
	}
	if len(retryable) > 0 {
		time.Sleep(500 * time.Millisecond)
		result, retryFailures, ok := t.fetchJSONRound(dedupeURLs(retryable))
		if ok {
			return result, nil
		}
		for _, failure := range retryFailures {
			failure.err = fmt.Errorf("retry: %w", failure.err)
			failures = append(failures, failure)
		}
	}
	return nil, joinSourceFailures(failures)
}

func (t *Translator) fetchJSONRound(urls []string) (any, []sourceFailure, bool) {
	type fetchResult struct {
		url  string
		data any
		err  error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan fetchResult, len(urls))
	next, active := 0, 0
	start := func(url string) {
		active++
		go func() {
			data, err := t.fetchJSONURLOnceContext(ctx, url)
			results <- fetchResult{url: url, data: data, err: err}
		}()
	}
	if len(urls) > 0 {
		start(urls[0])
		next = 1
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerC = nil
	}
	scheduleNext := func() {
		stopTimer()
		if next < len(urls) && active > 0 {
			timer = time.NewTimer(t.fetchHedgeDelay())
			timerC = timer.C
		}
	}
	scheduleNext()
	defer stopTimer()

	failures := make([]sourceFailure, 0, len(urls))
	for active > 0 || next < len(urls) {
		if active == 0 && next < len(urls) {
			stopTimer()
			start(urls[next])
			next++
			scheduleNext()
			continue
		}
		select {
		case result := <-results:
			active--
			if result.err == nil {
				cancel()
				return result.data, failures, true
			}
			failures = append(failures, sourceFailure{url: result.url, err: result.err})
			if active == 0 && next < len(urls) {
				stopTimer()
				start(urls[next])
				next++
				scheduleNext()
			}
		case <-timerC:
			timerC = nil
			if next < len(urls) {
				start(urls[next])
				next++
				scheduleNext()
			}
		}
	}
	return nil, failures, false
}

func (t *Translator) fetchJSONURLOnce(url string) (any, error) {
	return t.fetchJSONURLOnceContext(context.Background(), url)
}

func (t *Translator) fetchJSONURLOnceContext(ctx context.Context, url string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "moesekai-data-sync")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return nil, fmt.Errorf("GET %s: http %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("GET %s: gzip: %w", url, err)
		}
		defer zr.Close()
		reader = zr
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("GET %s: read: %w", url, err)
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", url, err)
	}
	return parsed, nil
}

// fetchJPScenarioJSON fetches a JP scenario from the configured source chain,
// requiring TalkData before accepting a response. Each source is attempted
// once before any transient retry, which makes failover immediate.
func (t *Translator) fetchJPScenarioJSON(assetPath string) (any, error) {
	urls := make([]string, 0, len(t.jpAssetBases()))
	for _, base := range t.jpAssetBases() {
		urls = append(urls, joinSourceURL(base, assetPath+".json"))
	}
	urls = dedupeURLs(urls)
	var incomplete any
	failures := make([]sourceFailure, 0, len(urls)*2)
	retryable := make([]string, 0, len(urls))
	for _, url := range urls {
		result, err := t.fetchJSONURLOnce(url)
		if err == nil && scenarioHasTalkData(result) {
			return result, nil
		}
		if err == nil {
			incomplete = result
			err = fmt.Errorf("GET %s: missing TalkData", url)
		}
		failures = append(failures, sourceFailure{url: url, err: err})
		if isTransientErr(err) {
			retryable = append(retryable, url)
		}
	}
	if len(retryable) > 0 {
		time.Sleep(500 * time.Millisecond)
		for _, url := range retryable {
			result, err := t.fetchJSONURLOnce(url)
			if err == nil && scenarioHasTalkData(result) {
				return result, nil
			}
			if err == nil {
				incomplete = result
				err = fmt.Errorf("GET %s: missing TalkData", url)
			}
			failures = append(failures, sourceFailure{url: url, err: fmt.Errorf("retry: %w", err)})
		}
	}
	if incomplete != nil {
		return incomplete, nil
	}
	return nil, joinSourceFailures(failures)
}

func (t *Translator) fetchCNScenarioJSON(assetPath string) (any, error) {
	return t.fetchJSONFromBases(t.cnAssetBases(), assetPath+".json")
}

func scenarioHasTalkData(data any) bool {
	m, ok := data.(map[string]any)
	if !ok {
		return false
	}
	arr, ok := m["TalkData"].([]any)
	return ok && len(arr) > 0
}

func joinSourceURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	path = strings.TrimLeft(strings.TrimSpace(path), "/")
	if base == "" {
		return ""
	}
	return base + "/" + path
}

func dedupeURLs(urls []string) []string {
	seen := make(map[string]bool, len(urls))
	out := make([]string, 0, len(urls))
	for _, url := range urls {
		url = strings.TrimSpace(url)
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		out = append(out, url)
	}
	return out
}

func joinSourceFailures(failures []sourceFailure) error {
	if len(failures) == 0 {
		return fmt.Errorf("all upstream sources failed")
	}
	parts := make([]string, 0, len(failures))
	for _, failure := range failures {
		parts = append(parts, failure.err.Error())
	}
	return fmt.Errorf("all upstream sources failed: %s", strings.Join(parts, "; "))
}

// isTransientErr reports whether an error is worth retrying (network/5xx).
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, code := range []string{"http 500", "http 502", "http 503", "http 504", "http 520", "http 521", "http 522", "http 523", "http 524", "http 525", "http 526", "http 527"} {
		if strings.Contains(s, code) {
			return true
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "connection refused")
}
