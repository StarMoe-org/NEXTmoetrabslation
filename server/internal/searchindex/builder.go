// Package searchindex regenerates the public search-index.json consumed by the
// site, combining upstream masterdata with the current translations. It is a
// port of the legacy backend/search_index.go, adapted to the SQLite store and
// the in-memory file service.
package searchindex

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/httpx"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

const (
	defaultJPMasterdataURL         = "https://metadata.pjsk.moe/jp/master"
	defaultJPMasterdataFallbackURL = "https://raw.githubusercontent.com/{repo}/{branch}/master"
)

type Entry struct {
	ID int    `json:"id"`
	N  string `json:"n"`
	G  string `json:"g"`
	C  int    `json:"c,omitempty"`
	CN string `json:"cn,omitempty"`
}

// Builder produces search-index.json and publishes it to the file service.
type Builder struct {
	store    *store.Store
	files    *filesvc.Service
	cfg      *config.Config
	client   *http.Client
	debounce time.Duration
	refresh  time.Duration

	triggerCh chan struct{}

	mu         sync.Mutex
	lastBuilt  time.Time
	lastResult string
}

func New(s *store.Store, fsvc *filesvc.Service, cfg *config.Config, debounce, refresh time.Duration) *Builder {
	if debounce <= 0 {
		debounce = time.Hour
	}
	if refresh <= 0 {
		refresh = time.Hour
	}
	return &Builder{
		store:     s,
		files:     fsvc,
		cfg:       cfg,
		client:    httpx.NewClient(60 * time.Second),
		debounce:  debounce,
		refresh:   refresh,
		triggerCh: make(chan struct{}, 1),
	}
}

// Start builds once immediately (in the background) and launches the debounce +
// periodic-refresh loop. Building on startup makes search-index.json available
// without waiting for the debounce window or the first refresh tick.
func (b *Builder) Start() {
	go b.loop()
	go b.build("startup")
}

// Trigger schedules a debounced rebuild.
func (b *Builder) Trigger() {
	select {
	case b.triggerCh <- struct{}{}:
	default:
	}
}

func (b *Builder) loop() {
	var timer *time.Timer
	var timerC <-chan time.Time
	ticker := time.NewTicker(b.refresh)
	defer ticker.Stop()
	for {
		select {
		case <-b.triggerCh:
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(b.debounce)
			timerC = timer.C
		case <-timerC:
			b.build("debounce")
			timerC = nil
		case <-ticker.C:
			b.build("refresh")
		}
	}
}

// catText looks up a translated cn text for a given field+jpText, returning ""
// if absent or unchanged from the source.
func (b *Builder) catText(cat model.Category, field, jp string) string {
	if cat == nil {
		return ""
	}
	if fm, ok := cat[field]; ok {
		if e, ok := fm[jp]; ok && e.Text != "" && e.Text != jp {
			return e.Text
		}
	}
	return ""
}

func (b *Builder) build(reason string) {
	index := make([]Entry, 0, 4096)
	successes := 0

	load := func(cat string) model.Category {
		c, err := b.store.CategoryData(cat)
		if err != nil {
			return nil
		}
		return c
	}
	eventsT := load("events")
	musicT := load("music")
	cardsT := load("cards")
	gachaT := load("gacha")
	mysekaiT := load("mysekai")
	costumesT := load("costumes")
	vlT := load("virtualLive")

	type src struct {
		file, group, nameField, transField string
		extraCharID                        bool
	}
	simple := []src{
		{"events.json", "events", "name", "name", false},
		{"musics.json", "music", "title", "title", false},
		{"cards.json", "cards", "prefix", "prefix", true},
		{"gachas.json", "gacha", "name", "name", false},
		{"mysekaiFixtures.json", "mysekai", "name", "fixtureName", false},
		{"virtualLives.json", "live", "name", "name", false},
	}
	transFor := map[string]model.Category{
		"events": eventsT, "music": musicT, "cards": cardsT,
		"gacha": gachaT, "mysekai": mysekaiT, "live": vlT,
	}

	for _, sdef := range simple {
		arr, err := b.fetchArray(sdef.file)
		if err != nil {
			fmt.Printf("[search-index] %s fetch failed: %v\n", sdef.file, err)
			continue
		}
		successes++
		for _, item := range arr {
			name, _ := item[sdef.nameField].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			e := Entry{ID: asInt(item["id"]), N: name, G: sdef.group}
			if sdef.extraCharID {
				e.C = asInt(item["characterId"])
			}
			if cn := b.catText(transFor[sdef.group], sdef.transField, name); cn != "" {
				e.CN = cn
			}
			index = append(index, e)
		}
	}

	// Costumes have a nested shape (snowy_costumes.json -> {costumes:[...]}).
	if costumes, err := b.fetchCostumes(); err != nil {
		fmt.Printf("[search-index] costumes fetch failed: %v\n", err)
	} else {
		successes++
		for _, c := range costumes {
			name, _ := c["name"].(string)
			if strings.TrimSpace(name) == "" || name == "-" {
				continue
			}
			e := Entry{ID: asInt(c["id"]), N: name, G: "costumes"}
			if cn := b.catText(costumesT, "name", name); cn != "" {
				e.CN = cn
			}
			index = append(index, e)
		}
	}

	if successes == 0 {
		fmt.Printf("[search-index] all fetches failed; keeping existing index\n")
		return
	}

	buf, err := json.Marshal(index)
	if err != nil {
		fmt.Printf("[search-index] marshal failed: %v\n", err)
		return
	}
	b.files.SetAsset("data/search-index.json", buf, "application/json; charset=utf-8")
	b.mu.Lock()
	b.lastBuilt = time.Now()
	b.lastResult = fmt.Sprintf("%d entries (%s)", len(index), reason)
	b.mu.Unlock()
	fmt.Printf("[search-index] published %d entries (reason=%s)\n", len(index), reason)
}

func (b *Builder) fetchArray(filename string) ([]map[string]any, error) {
	data, err := b.fetchMasterdata(filename)
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

func (b *Builder) fetchCostumes() ([]map[string]any, error) {
	data, err := b.fetchMasterdata("snowy_costumes.json")
	if err != nil {
		return nil, err
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected json type for snowy_costumes.json")
	}
	arr, ok := obj["costumes"].([]any)
	if !ok {
		return nil, fmt.Errorf("missing costumes array")
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (b *Builder) fetchMasterdata(filename string) (any, error) {
	bases := []string{defaultJPMasterdataURL, defaultJPMasterdataFallbackURL}
	repo, branch := "Team-Haruki/haruki-sekai-master", "main"
	if b.cfg != nil {
		bases[0] = b.cfg.GetOr(config.KeyUpstreamJPMasterdataURL, bases[0])
		bases[1] = b.cfg.GetOr(config.KeyUpstreamJPMasterdataFallbackURL, bases[1])
		repo = b.cfg.GetOr(config.KeyUpstreamRepo, repo)
		branch = b.cfg.GetOr(config.KeyUpstreamBranch, branch)
	}
	seen := map[string]bool{}
	var failures []string
	for _, base := range bases {
		base = strings.TrimRight(strings.TrimSpace(base), "/")
		base = strings.ReplaceAll(base, "{repo}", repo)
		base = strings.ReplaceAll(base, "{branch}", branch)
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		url := base + "/" + strings.TrimLeft(filename, "/")
		data, err := b.fetchJSON(url)
		if err == nil {
			return data, nil
		}
		failures = append(failures, err.Error())
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("no JP masterdata source configured")
	}
	return nil, fmt.Errorf("all JP masterdata sources failed: %s", strings.Join(failures, "; "))
}

func (b *Builder) fetchJSON(url string) (any, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", "moesekai-search-index")
	resp, err := b.client.Do(req)
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
			return nil, err
		}
		defer zr.Close()
		reader = zr
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func asInt(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	case string:
		var n int
		fmt.Sscanf(v, "%d", &n)
		return n
	default:
		return 0
	}
}
