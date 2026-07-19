package translator

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/httpx"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// ProgressFn receives progress notes during long-running operations (wired to
// SSE by the caller). stage is a short label; detail is human-readable.
type ProgressFn func(stage, detail string, current, total int)

// Translator runs CN sync + AI translation against the SQLite store. Config
// (LLM keys, models, batch size) is read live from the config store so admin
// changes take effect without restart.
type Translator struct {
	store      *store.Store
	eventStore *store.EventStore
	cfg        *config.Config
	dataClient *http.Client
	llmClient  *http.Client
	hedgeDelay time.Duration

	mu       sync.Mutex
	status   Status
	progress ProgressFn

	llmUnavailableUntil time.Time
	llmLastError        string
}

// Status reports the translator's current run state.
type Status struct {
	Running   bool   `json:"running"`
	LastRun   string `json:"lastRun,omitempty"`
	LastMode  string `json:"lastMode,omitempty"`
	LastError string `json:"lastError,omitempty"`
	LastNote  string `json:"lastNote,omitempty"`
}

// llmConfig is a snapshot of LLM settings for one operation.
type llmConfig struct {
	LLMType        string
	GeminiAPIKey   string
	GeminiModel    string
	OpenAIAPIKey   string
	OpenAIBaseURL  string
	OpenAIModel    string
	RequestTimeout time.Duration
	MaxRetries     int
	BatchSize      int
	RateDelay      time.Duration
}

const (
	defaultDataRequestTimeout = 3 * time.Minute
	defaultLLMRequestTimeout  = 45 * time.Second
	defaultLLMMaxRetries      = 2
	automaticLLMTimeout       = 30 * time.Second
	llmFailureCooldown        = 10 * time.Minute
)

func New(s *store.Store, es *store.EventStore, cfg *config.Config) *Translator {
	return &Translator{
		store:      s,
		eventStore: es,
		cfg:        cfg,
		dataClient: httpx.NewClientWithTimeouts(defaultDataRequestTimeout, 15*time.Second, 25*time.Second, 45*time.Second),
		// LLM calls use a live, per-request context timeout from config. Keeping
		// the client timeout at zero prevents it from fighting that deadline,
		// especially while an OpenAI-compatible SSE response is streaming.
		llmClient:  httpx.NewClientWithTimeouts(0, 10*time.Second, 15*time.Second, 0),
		hedgeDelay: defaultSourceHedgeDelay,
	}
}

// SetProgress installs a progress callback (e.g. SSE broadcast).
func (t *Translator) SetProgress(fn ProgressFn) { t.progress = fn }

func (t *Translator) emit(stage, detail string, cur, total int) {
	if t.progress != nil {
		t.progress(stage, detail, cur, total)
	}
}

// snapshotConfig reads current LLM settings from the config store.
func (t *Translator) snapshotConfig() llmConfig {
	requestTimeout := time.Duration(t.cfg.GetInt(config.KeyLLMRequestTimeoutMS, int(defaultLLMRequestTimeout/time.Millisecond))) * time.Millisecond
	if requestTimeout <= 0 {
		requestTimeout = defaultLLMRequestTimeout
	}
	maxRetries := t.cfg.GetInt(config.KeyLLMMaxRetries, defaultLLMMaxRetries)
	if maxRetries < 0 {
		maxRetries = 0
	}
	if maxRetries > 5 {
		maxRetries = 5
	}
	return llmConfig{
		LLMType:        t.cfg.GetOr(config.KeyLLMType, "openai"),
		GeminiAPIKey:   t.cfg.Get(config.KeyGeminiAPIKey),
		GeminiModel:    t.cfg.GetOr(config.KeyGeminiModel, "gemini-2.0-flash"),
		OpenAIAPIKey:   t.cfg.Get(config.KeyOpenAIAPIKey),
		OpenAIBaseURL:  t.cfg.GetOr(config.KeyOpenAIBaseURL, "https://api.openai.com/v1"),
		OpenAIModel:    t.cfg.GetOr(config.KeyOpenAIModel, "gpt-4o-mini"),
		RequestTimeout: requestTimeout,
		MaxRetries:     maxRetries,
		BatchSize:      t.cfg.GetInt(config.KeyBatchSize, 20),
		RateDelay:      time.Duration(t.cfg.GetInt(config.KeyRateDelayMS, 800)) * time.Millisecond,
	}
}

func (t *Translator) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status
}

func (t *Translator) recordLLMSuccess() {
	t.mu.Lock()
	t.llmUnavailableUntil = time.Time{}
	t.llmLastError = ""
	t.mu.Unlock()
}

func (t *Translator) recordLLMFailure(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	t.llmUnavailableUntil = time.Now().Add(llmFailureCooldown)
	t.llmLastError = err.Error()
	t.mu.Unlock()
}

func (t *Translator) automaticLLMUnavailable() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.llmUnavailableUntil.IsZero() || !time.Now().Before(t.llmUnavailableUntil) {
		t.llmUnavailableUntil = time.Time{}
		t.llmLastError = ""
		return "", false
	}
	return fmt.Sprintf("LLM 暂时不可用（冷却至 %s）：%s", t.llmUnavailableUntil.UTC().Format(time.RFC3339), t.llmLastError), true
}

// markStart claims the single-run lock, returning an error if already running.
func (t *Translator) markStart(mode string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status.Running {
		log.Printf("[translate] %s rejected: a job is already running", mode)
		return fmt.Errorf("a translate job is already running")
	}
	t.status.Running = true
	t.status.LastMode = mode
	t.status.LastError = ""
	log.Printf("[translate] %s started", mode)
	return nil
}

func (t *Translator) setNote(note string) {
	t.mu.Lock()
	t.status.LastNote = note
	t.mu.Unlock()
}

func (t *Translator) markEnd(note string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	mode := t.status.LastMode
	t.status.Running = false
	t.status.LastRun = time.Now().UTC().Format(time.RFC3339)
	t.status.LastNote = note
	if err != nil {
		t.status.LastError = err.Error()
		log.Printf("[translate] %s FAILED: %v", mode, err)
	} else {
		log.Printf("[translate] %s done: %s", mode, note)
	}
}

// IsAlreadyRunning reports whether an error is the "already running" sentinel.
func IsAlreadyRunning(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "already running")
}

// ---- CN sync ----

// CNSyncResult summarizes a CN-sync run.
type CNSyncResult struct {
	Mode                 string            `json:"mode"`
	Categories           int               `json:"categories"`
	UpdatedEntries       int               `json:"updatedEntries"`
	EventStoryFiles      int               `json:"eventStoryFiles"`
	AITranslationSkipped int               `json:"aiTranslationSkipped,omitempty"`
	AITranslationNote    string            `json:"aiTranslationNote,omitempty"`
	Skipped              []string          `json:"skipped,omitempty"`
	SkippedDetails       map[string]string `json:"skippedDetails,omitempty"`
}

func (r *CNSyncResult) addSkipped(category string, err error) {
	seen := false
	for _, existing := range r.Skipped {
		if existing == category {
			seen = true
			break
		}
	}
	if !seen {
		r.Skipped = append(r.Skipped, category)
	}
	if err == nil {
		return
	}
	if r.SkippedDetails == nil {
		r.SkippedDetails = map[string]string{}
	}
	r.SkippedDetails[category] = err.Error()
	log.Printf("[translate] cn-sync skipped %s: %v", category, err)
}

// SkippedError returns a concise, actionable status error suitable for the
// upstream watcher and management UI. Full details remain in SkippedDetails.
func (r CNSyncResult) SkippedError() error {
	if len(r.Skipped) == 0 {
		return nil
	}
	parts := make([]string, 0, len(r.Skipped))
	for _, category := range r.Skipped {
		part := category
		if detail := strings.TrimSpace(r.SkippedDetails[category]); detail != "" {
			part += ": " + truncateStatusDetail(detail, 280)
		}
		parts = append(parts, part)
	}
	return fmt.Errorf("data sync skipped: %s", strings.Join(parts, "; "))
}

func truncateStatusDetail(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

func summarizeErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	limit := len(errs)
	if limit > 3 {
		limit = 3
	}
	parts := make([]string, 0, limit)
	for _, err := range errs[:limit] {
		parts = append(parts, err.Error())
	}
	if len(errs) > limit {
		parts = append(parts, fmt.Sprintf("另有 %d 个错误", len(errs)-limit))
	}
	return fmt.Errorf("%d 个剧情子任务失败: %s", len(errs), strings.Join(parts, "; "))
}

// SyncCNOnly fetches masterdata and applies official CN translations to all
// categories plus event stories. It is the scheduled / manual "数据更新" action.
func (t *Translator) SyncCNOnly() (CNSyncResult, error) {
	if err := t.markStart("cn-sync"); err != nil {
		return CNSyncResult{}, err
	}
	result := CNSyncResult{Mode: "cn-sync"}
	var runErr error
	defer func() {
		note := "cn sync complete"
		if runErr != nil {
			note = "cn sync failed"
		} else if result.AITranslationSkipped > 0 {
			note = fmt.Sprintf("cn sync complete; skipped AI translation for %d event stories", result.AITranslationSkipped)
		}
		t.markEnd(note, runErr)
	}()

	steps := []struct {
		category string
		fn       func() (map[string]store.CNApplyField, error)
	}{
		{"cards", t.extractCards},
		{"skills", t.extractSkills},
		{"events", t.extractEvents},
		{"information", t.extractInformation},
		{"gacha", t.extractGacha},
		{"virtualLive", t.extractVirtualLive},
		{"sticker", t.extractStickers},
		{"comic", t.extractComics},
		{"mysekai", t.extractMysekai},
		{"costumes", t.extractCostumes},
		{"characters", t.extractCharacters},
		{"units", t.extractUnits},
		{"music", t.extractMusic},
	}

	// Remote extraction is read-only and independent per category. Fetch a
	// bounded number in parallel, then apply results to SQLite in the stable
	// category order below. This keeps database writes serialized while avoiding
	// dozens of latency-bound HTTP requests running one after another.
	type stepResult struct {
		fields map[string]store.CNApplyField
		err    error
	}
	fetched := make([]stepResult, len(steps))
	jobs := make(chan int)
	done := make(chan int, len(steps))
	workers := t.fetchConcurrency()
	if workers > len(steps) {
		workers = len(steps)
	}
	var fetchWG sync.WaitGroup
	for range workers {
		fetchWG.Add(1)
		go func() {
			defer fetchWG.Done()
			for i := range jobs {
				fetched[i].fields, fetched[i].err = steps[i].fn()
				done <- i
			}
		}()
	}
	go func() {
		for i := range steps {
			jobs <- i
		}
		close(jobs)
		fetchWG.Wait()
		close(done)
	}()

	progressTotal := len(steps)*2 + 2
	t.setNote("cn-sync fetching masterdata")
	fetchedCount := 0
	for i := range done {
		fetchedCount++
		t.emit("sync.progress", "已拉取 "+steps[i].category, fetchedCount, progressTotal)
	}

	for i, step := range steps {
		t.setNote(fmt.Sprintf("cn-sync %d/%d: %s", i+1, len(steps), step.category))
		t.emit("sync.progress", "正在写入 "+step.category, len(steps)+i+1, progressTotal)
		fields, err := fetched[i].fields, fetched[i].err
		if err != nil {
			if isTransientErr(err) {
				result.addSkipped(step.category, err)
				continue
			}
			runErr = fmt.Errorf("%s: %w", step.category, err)
			return result, runErr
		}
		updated, err := t.store.ApplyCNCategory(step.category, fields)
		if err != nil {
			runErr = fmt.Errorf("apply %s: %w", step.category, err)
			return result, runErr
		}
		result.Categories++
		result.UpdatedEntries += updated
	}

	t.setNote("cn-sync event stories")
	storyProgress := progressTotal - 1
	t.emit("sync.progress", "正在更新活动剧情", storyProgress, progressTotal)
	storyOutcome, err := t.syncEventStoriesCNOnly(storyProgress, progressTotal)
	if err != nil {
		if isTransientErr(err) {
			result.addSkipped("eventStories", err)
		} else {
			runErr = err
			return result, runErr
		}
	} else {
		result.EventStoryFiles = storyOutcome.Processed
		result.AITranslationSkipped = storyOutcome.AITranslationSkipped
		result.AITranslationNote = storyOutcome.AITranslationNote
		if len(storyOutcome.PartialErrors) > 0 {
			result.addSkipped("eventStories", summarizeErrors(storyOutcome.PartialErrors))
		}
	}
	t.emit("sync.progress", "数据更新完成", progressTotal, progressTotal)
	return result, nil
}

// ---- AI translation ----

// AITranslateRequest targets one category/field for LLM gap-filling.
type AITranslateRequest struct {
	Category string `json:"category"`
	Field    string `json:"field"`
	Provider string `json:"provider"`
	Limit    int    `json:"limit"`
}

// AITranslateResult summarizes an AI translation run for one field.
type AITranslateResult struct {
	Category        string `json:"category"`
	Field           string `json:"field"`
	Provider        string `json:"provider"`
	Candidates      int    `json:"candidates"`
	Translated      int    `json:"translated"`
	SkippedExisting int    `json:"skippedExisting"`
}

// ManualAITranslate fills empty entries in one field via the LLM.
func (t *Translator) ManualAITranslate(req AITranslateRequest) (AITranslateResult, error) {
	if err := t.markStart("manual-ai"); err != nil {
		return AITranslateResult{}, err
	}
	var runErr error
	defer func() { t.markEnd("manual ai complete", runErr) }()

	provider := normalizeProvider(req.Provider, t.cfg.GetOr(config.KeyLLMType, "openai"))
	result := AITranslateResult{Category: req.Category, Field: req.Field, Provider: provider}

	if req.Category == "" || req.Field == "" {
		runErr = fmt.Errorf("category and field are required")
		return result, runErr
	}
	if !model.IsValidCategory(req.Category) {
		runErr = fmt.Errorf("unsupported category: %s", req.Category)
		return result, runErr
	}
	if provider != "gemini" && provider != "openai" {
		runErr = fmt.Errorf("unsupported provider: %s", provider)
		return result, runErr
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 200
	}
	candidates, skipped, err := t.store.AICandidates(req.Category, req.Field, limit)
	if err != nil {
		runErr = err
		return result, runErr
	}
	result.SkippedExisting = skipped
	result.Candidates = len(candidates)
	if len(candidates) == 0 {
		return result, nil
	}
	sort.Strings(candidates)

	updates, translateErr := t.translateBatch(provider, candidates)
	if len(updates) > 0 {
		translated, moreSkipped, err := t.store.ApplyAITranslations(req.Category, req.Field, updates)
		if err != nil {
			runErr = err
			return result, runErr
		}
		result.Translated = translated
		result.SkippedExisting += moreSkipped
	}
	if translateErr != nil {
		runErr = translateErr
		return result, runErr
	}
	return result, nil
}

// translateBatch runs LLM translation over keys in BatchSize chunks, honoring
// the rate-limit delay. Returns jp -> cn for non-empty results.
func (t *Translator) translateBatch(provider string, keys []string) (map[string]string, error) {
	cfg := t.snapshotConfig()
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}
	updates := make(map[string]string, len(keys))
	for i := 0; i < len(keys); i += batchSize {
		end := i + batchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]
		log.Printf("[translate] batch %d-%d/%d (provider=%s)", i+1, end, len(keys), provider)
		translated, err := t.callLLMWithAttempts(provider, batch, func(attempt, attempts int) {
			t.emit("translate.progress", fmt.Sprintf("AI 翻译中 %d/%d · 请求 %d/%d", i, len(keys), attempt, attempts), i, len(keys))
		})
		if err != nil {
			log.Printf("[translate] batch %d-%d failed: %v", i+1, end, err)
			return updates, err
		}
		for idx, jp := range batch {
			if idx < len(translated) {
				if cn := strings.TrimSpace(translated[idx]); cn != "" {
					updates[jp] = cn
				}
			}
		}
		t.emit("translate.progress", fmt.Sprintf("AI 翻译已完成 %d/%d", end, len(keys)), end, len(keys))
		if end < len(keys) {
			time.Sleep(cfg.RateDelay)
		}
	}
	return updates, nil
}

func normalizeProvider(provider, fallback string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		p = strings.ToLower(strings.TrimSpace(fallback))
	}
	return p
}
