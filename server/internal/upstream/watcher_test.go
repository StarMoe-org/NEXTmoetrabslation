package upstream

import (
	"testing"
	"time"
)

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
