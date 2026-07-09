// Package translator ports the legacy CN-sync + AI translation engine to the
// SQLite-backed store. It fetches masterdata from the JP/CN mirrors, extracts
// translatable text per category, applies official CN translations, and fills
// gaps with LLM translation (Gemini / OpenAI-compatible).
package translator

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Mirror endpoints.
const (
	jpMasterdataURL     = "https://metadata.pjsk.moe/jp/master"
	cnMasterdataURL     = "https://metadata.pjsk.moe/cn/master"
	jpAssetsURL         = "https://snowyassets.exmeaning.com/ondemand"
	jpAssetsFallbackURL = "https://assets.unipjsk.com/ondemand"
	cnAssetsURL         = "https://sekai-assets-bdf29c81.seiunx.net/cn-assets/ondemand"
)

// fetchMasterdata fetches a masterdata array from the jp or cn mirror.
func (t *Translator) fetchMasterdata(filename, server string) ([]map[string]any, error) {
	base := jpMasterdataURL
	if server == "cn" {
		base = cnMasterdataURL
	}
	data, err := t.fetchJSONURL(fmt.Sprintf("%s/%s", base, filename))
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

// fetchJSONURL fetches and decodes JSON, retrying transient errors.
func (t *Translator) fetchJSONURL(url string) (any, error) {
	const maxRetries = 2
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, err := t.fetchJSONURLOnce(url)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isTransientErr(err) {
			return nil, err
		}
		if attempt < maxRetries {
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
		}
	}
	return nil, lastErr
}

func (t *Translator) fetchJSONURLOnce(url string) (any, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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

// fetchJPScenarioJSON fetches a JP scenario, validating TalkData is present and
// falling back to the backup CDN when the primary returns empty/incomplete data.
func (t *Translator) fetchJPScenarioJSON(assetPath string) (any, error) {
	primaryURL := fmt.Sprintf("%s/%s.json", jpAssetsURL, assetPath)
	result, err := t.fetchJSONURL(primaryURL)
	if err == nil && scenarioHasTalkData(result) {
		return result, nil
	}
	fallbackURL := fmt.Sprintf("%s/%s.json", jpAssetsFallbackURL, assetPath)
	fallbackResult, fallbackErr := t.fetchJSONURL(fallbackURL)
	if fallbackErr != nil {
		if err == nil && result != nil {
			return result, nil // both incomplete; use primary
		}
		return nil, fmt.Errorf("primary: %v; fallback: %v", err, fallbackErr)
	}
	return fallbackResult, nil
}

func scenarioHasTalkData(data any) bool {
	m, ok := data.(map[string]any)
	if !ok {
		return false
	}
	arr, ok := m["TalkData"].([]any)
	return ok && len(arr) > 0
}

// isTransientErr reports whether an error is worth retrying (network/5xx).
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if strings.Contains(s, "http 502") || strings.Contains(s, "http 503") || strings.Contains(s, "http 504") {
		return true
	}
	var netErr net.Error
	if ok := asNetErr(err, &netErr); ok {
		return true
	}
	return strings.Contains(s, "connection reset") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "connection refused")
}

func asNetErr(err error, target *net.Error) bool {
	if ne, ok := err.(net.Error); ok {
		*target = ne
		return true
	}
	return false
}
