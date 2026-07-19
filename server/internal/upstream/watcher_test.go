package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
)

func openWatcherConfig(t *testing.T) *config.Config {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/watcher.db")
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

func TestParseRetryAfterSeconds(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	got := parseRetryAfter("120", now)
	if got != 2*time.Minute {
		t.Fatalf("parseRetryAfter seconds = %s, want 2m", got)
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	got := parseRetryAfter("Thu, 09 Jul 2026 12:30:00 GMT", now)
	if got != 30*time.Minute {
		t.Fatalf("parseRetryAfter date = %s, want 30m", got)
	}
}

func TestRateLimitCooldownFallsBackToAtLeastOneHour(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	got := rateLimitCooldown("", now, 5*time.Minute)
	if got != time.Hour {
		t.Fatalf("rateLimitCooldown fallback = %s, want 1h", got)
	}
}

func TestRateLimitCooldownCapsFallback(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	got := rateLimitCooldown("", now, 12*time.Hour)
	if got != maxFallbackCooldown {
		t.Fatalf("rateLimitCooldown capped fallback = %s, want %s", got, maxFallbackCooldown)
	}
}

func TestExpandVersionURLDefaultsToMirror(t *testing.T) {
	got := expandVersionURL("", "owner/repo", "main")
	want := "https://metadata.pjsk.moe/jp/versions/current_version.json"
	if got != want {
		t.Fatalf("expandVersionURL default = %q, want %q", got, want)
	}
}

func TestExpandVersionURLTemplate(t *testing.T) {
	got := expandVersionURL("https://cdn.jsdelivr.net/gh/{repo}@{branch}/versions/current_version.json", "owner/repo", "dev")
	want := "https://cdn.jsdelivr.net/gh/owner/repo@dev/versions/current_version.json"
	if got != want {
		t.Fatalf("expandVersionURL template = %q, want %q", got, want)
	}
}

func TestFetchVersionFallsBackToSecondarySource(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })

	primaryCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls++
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer primary.Close()

	fallbackCalls := 0
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"appVersion":"1","dataVersion":"2","assetVersion":"3"}`)
	}))
	defer fallback.Close()

	cfg := openWatcherConfig(t)
	if err := cfg.Set(config.KeyUpstreamVersionURL, primary.URL); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(config.KeyUpstreamVersionFallbackURL, fallback.URL); err != nil {
		t.Fatal(err)
	}
	w := New(cfg, nil, Options{})

	info, source, err := w.fetchVersion()
	if err != nil {
		t.Fatal(err)
	}
	if info.DataVersion != "2" || source != fallback.URL {
		t.Fatalf("unexpected fallback result: info=%+v source=%q", info, source)
	}
	if primaryCalls != 1 || fallbackCalls != 1 {
		t.Fatalf("unexpected calls: primary=%d fallback=%d", primaryCalls, fallbackCalls)
	}
}

func TestSplitVersionTemplates(t *testing.T) {
	got := splitVersionTemplates("https://one.example/a, https://two.example/b\nhttps://three.example/c")
	if len(got) != 3 {
		t.Fatalf("unexpected templates: %v", got)
	}
}

func TestFetchVersionRacesSlowPrimary(t *testing.T) {
	oldBuiltIns := builtInVersionFallbackURLs
	builtInVersionFallbackURLs = nil
	t.Cleanup(func() { builtInVersionFallbackURLs = oldBuiltIns })

	primary := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"dataVersion":"fast"}`)
	}))
	defer fallback.Close()

	cfg := openWatcherConfig(t)
	cfg.Set(config.KeyUpstreamVersionURL, primary.URL)
	cfg.Set(config.KeyUpstreamVersionFallbackURL, fallback.URL)
	w := New(cfg, nil, Options{})

	started := time.Now()
	info, source, err := w.fetchVersion()
	if err != nil {
		t.Fatal(err)
	}
	if info.DataVersion != "fast" || source != fallback.URL || time.Since(started) > time.Second {
		t.Fatalf("slow primary was not raced: info=%+v source=%q elapsed=%s", info, source, time.Since(started))
	}
}

func TestRecordSyncSuccessClearsStaleError(t *testing.T) {
	cfg := openWatcherConfig(t)
	w := New(cfg, nil, Options{})
	w.setStatus(func(s *Status) {
		s.LastError = "old timeout"
		s.LastErrorAt = "old"
		s.ConsecutiveFailures = 2
	})

	w.RecordSyncResult(nil)
	status := w.Status()
	if status.LastError != "" || status.LastErrorAt != "" || status.ConsecutiveFailures != 0 {
		t.Fatalf("stale error was not cleared: %+v", status)
	}
	if status.LastSync == "" || status.LastSuccess == "" {
		t.Fatalf("sync timestamps not recorded: %+v", status)
	}
}
