package translator

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
)

func openTranslatorConfig(t *testing.T) *config.Config {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/translator.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	cfg, err := config.New(database, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBuildAndParseXMLRoundTrip(t *testing.T) {
	texts := []string{"こんにちは", "A & B", "<tag>", "三", ""}
	xml := buildXMLInput(texts)
	// Simulate an LLM echoing translations back in the expected format.
	resp := "<translations>"
	for i := range texts {
		resp += "<t id=\"" + strconv.Itoa(i+1) + "\">译" + strconv.Itoa(i+1) + "</t>"
	}
	resp += "</translations>"
	got := parseXMLTranslations(resp, len(texts))
	want := []string{"译1", "译2", "译3", "译4", "译5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse mismatch:\n got %v\nwant %v\nxml=%s", got, want, xml)
	}
}

func TestParseXMLStripsThinkAndHandlesGaps(t *testing.T) {
	content := `<think>reasoning here</think><t id="1">甲</t><t id="3">丙</t>`
	got := parseXMLTranslations(content, 3)
	want := []string{"甲", "", "丙"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestXMLEscapeUnescapeRoundTrip(t *testing.T) {
	in := `a & b < c > d`
	if out := xmlUnescape(xmlEscape(in)); out != in {
		t.Errorf("round trip failed: %q -> %q", in, out)
	}
}

func TestCollectPairBlanksWhenEqual(t *testing.T) {
	m := map[string]string{}
	collectPair(m, "同じ", "同じ") // jp==cn means untranslated
	if m["同じ"] != "" {
		t.Errorf("expected blank cn when jp==cn, got %q", m["同じ"])
	}
	collectPair(m, "日本語", "中文")
	if m["日本語"] != "中文" {
		t.Errorf("expected translated value, got %q", m["日本語"])
	}
}

func TestTraceMapDedup(t *testing.T) {
	tm := newTraceMap("name")
	tm.add("name", "テスト", 1)
	tm.add("name", "テスト", 1) // duplicate
	tm.add("name", "テスト", 2)
	tm.add("name", "", 3)    // empty jp ignored
	tm.add("name", "テスト", 0) // zero id ignored
	if got := tm["name"]["テスト"]; !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("trace dedup: got %v", got)
	}
}

func TestNormalizeStorySource(t *testing.T) {
	cases := map[string]string{
		"official_cn":        "official_cn",
		"official_cn_legacy": "official_cn",
		"cn":                 "official_cn",
		"llm":                "llm",
		"human":              "human",
		"jp_pending":         "jp_pending",
		"":                   "jp_pending",
	}
	for in, want := range cases {
		if got := normalizeStorySource(in); got != want {
			t.Errorf("normalizeStorySource(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchJPScenarioUsesHealthyFallbackImmediately(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(525)
	}))
	defer primary.Close()

	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"TalkData":[{"Body":"hello"}]}`)
	}))
	defer fallback.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPAssetsURL, primary.URL)
	cfg.Set(config.KeyUpstreamJPAssetsFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)

	result, err := tr.fetchJPScenarioJSON("event_story/test/scenario/1")
	if err != nil {
		t.Fatal(err)
	}
	if !scenarioHasTalkData(result) {
		t.Fatalf("fallback result missing TalkData: %#v", result)
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("dead primary was retried before fallback: primary=%d fallback=%d", primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestDefaultJPAssetSourceIsHealthyMirror(t *testing.T) {
	tr := &Translator{}
	bases := tr.jpAssetBases()
	if len(bases) == 0 || bases[0] != "https://assets.unipjsk.com/ondemand" {
		t.Fatalf("unexpected default JP asset sources: %v", bases)
	}
}

func TestMasterdataFallsBackToSecondarySource(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer primary.Close()

	var fallbackCalls atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":1}]`)
	}))
	defer fallback.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPMasterdataURL, primary.URL)
	cfg.Set(config.KeyUpstreamJPMasterdataFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)

	items, err := tr.fetchMasterdata("events.json", "jp")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("unexpected fallback result: items=%v primary=%d fallback=%d", items, primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestMasterdataHedgesSlowPrimary(t *testing.T) {
	primaryStarted := make(chan struct{}, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case primaryStarted <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":1}]`)
	}))
	defer fallback.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPMasterdataURL, primary.URL)
	cfg.Set(config.KeyUpstreamJPMasterdataFallbackURL, fallback.URL)
	tr := New(nil, nil, cfg)
	tr.hedgeDelay = 20 * time.Millisecond

	started := time.Now()
	items, err := tr.fetchMasterdata("events.json", "jp")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || time.Since(started) > time.Second {
		t.Fatalf("slow primary was not hedged promptly: items=%v elapsed=%s", items, time.Since(started))
	}
	select {
	case <-primaryStarted:
	default:
		t.Fatal("primary source was not attempted")
	}
}

func TestBuildJPPendingEpisodesFetchesConcurrently(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			seen := maxActive.Load()
			if current <= seen || maxActive.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"TalkData":[{"Body":"line","WindowDisplayName":"name"}]}`)
	}))
	defer server.Close()

	cfg := openTranslatorConfig(t)
	cfg.Set(config.KeyUpstreamJPAssetsURL, server.URL)
	cfg.Set(config.KeyUpstreamFetchConcurrency, "4")
	tr := New(nil, nil, cfg)
	story := map[string]any{
		"assetbundleName": "asset",
		"eventStoryEpisodes": []any{
			map[string]any{"episodeNo": float64(1), "scenarioId": "one", "title": "1"},
			map[string]any{"episodeNo": float64(2), "scenarioId": "two", "title": "2"},
			map[string]any{"episodeNo": float64(3), "scenarioId": "three", "title": "3"},
			map[string]any{"episodeNo": float64(4), "scenarioId": "four", "title": "4"},
		},
	}

	episodes, errs := tr.buildJPPendingEpisodes(story)
	if errs != 0 || len(episodes) != 4 {
		t.Fatalf("unexpected build result: episodes=%d errors=%d", len(episodes), errs)
	}
	if maxActive.Load() < 2 {
		t.Fatalf("scenario requests were not concurrent; max=%d", maxActive.Load())
	}
}
