package translator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const gameContextPrompt = "你是一个专业的游戏翻译器，专门翻译《世界计划 彩色舞台 feat. 初音未来》(Project SEKAI) 游戏内容。\n请将以下XML格式的日文文本翻译成简体中文。\n请只返回<translations>...</translations>，每条使用 <t id=\"N\">文本</t>。\n"

// callLLM translates a batch of JP texts via the given provider. Returns a
// slice aligned to texts (empty string where unparsed).
func (t *Translator) callLLM(provider string, texts []string) ([]string, error) {
	return t.callLLMWithAttempts(provider, texts, nil)
}

// callLLMWithAttempts is callLLM with an optional callback invoked before each
// network attempt. max_retries is the number of retries after the first call.
func (t *Translator) callLLMWithAttempts(provider string, texts []string, onAttempt func(attempt, total int)) ([]string, error) {
	return t.callLLMUsingConfig(provider, texts, t.snapshotConfig(), onAttempt)
}

// callAutomaticLLM is deliberately fail-fast because AI is optional during a
// data sync. A manual translation can later retry with the configured timeout
// and retry budget, resuming from the batches already saved.
func (t *Translator) callAutomaticLLM(provider string, texts []string, onAttempt func(attempt, total int)) ([]string, error) {
	cfg := t.snapshotConfig()
	if cfg.RequestTimeout > automaticLLMTimeout {
		cfg.RequestTimeout = automaticLLMTimeout
	}
	cfg.MaxRetries = 0
	return t.callLLMUsingConfig(provider, texts, cfg, onAttempt)
}

func (t *Translator) callLLMUsingConfig(provider string, texts []string, cfg llmConfig, onAttempt func(attempt, total int)) ([]string, error) {
	if len(texts) == 0 {
		return []string{}, nil
	}
	attempts := cfg.MaxRetries + 1
	prompt := gameContextPrompt + buildXMLInput(texts)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if onAttempt != nil {
			onAttempt(attempt, attempts)
		}
		ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
		var content string
		var err error
		switch provider {
		case "gemini":
			content, err = t.callGemini(ctx, prompt, cfg)
		case "openai":
			content, err = t.callOpenAI(ctx, prompt, cfg)
		default:
			cancel()
			return nil, fmt.Errorf("unsupported provider: %s", provider)
		}
		cancel()
		if err != nil {
			lastErr = err
			log.Printf("[llm] %s attempt %d/%d request error: %v", provider, attempt, attempts, err)
			if attempt < attempts {
				time.Sleep(time.Duration(attempt) * time.Second)
			}
			continue
		}
		parsed := parseXMLTranslations(content, len(texts))
		nonEmpty := 0
		for _, s := range parsed {
			if strings.TrimSpace(s) != "" {
				nonEmpty++
			}
		}
		minimum := (len(texts) + 1) / 2
		if nonEmpty >= minimum {
			t.recordLLMSuccess()
			return parsed, nil
		}
		lastErr = fmt.Errorf("parse incomplete: %d non-empty of %d", nonEmpty, len(texts))
		log.Printf("[llm] %s attempt %d/%d %v", provider, attempt, attempts, lastErr)
		if attempt < attempts {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	log.Printf("[llm] %s gave up after %d attempts (texts=%d): %v", provider, attempts, len(texts), lastErr)
	t.recordLLMFailure(lastErr)
	return nil, fmt.Errorf("llm failed after %d attempts (provider=%s, texts=%d): %w", attempts, provider, len(texts), lastErr)
}

func (t *Translator) callGemini(ctx context.Context, prompt string, cfg llmConfig) (string, error) {
	if strings.TrimSpace(cfg.GeminiAPIKey) == "" {
		return "", fmt.Errorf("GEMINI_API_KEY is not configured")
	}
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", cfg.GeminiModel)
	payload := map[string]any{
		"contents": []map[string]any{{"parts": []map[string]string{{"text": prompt}}}},
		"generationConfig": map[string]any{
			"temperature":      0.3,
			"maxOutputTokens":  8192,
			"candidateCount":   1,
			"responseMimeType": "text/plain",
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", cfg.GeminiAPIKey)
	resp, err := t.llmClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini returned empty candidates")
	}
	return result.Candidates[0].Content.Parts[0].Text, nil
}

func (t *Translator) callOpenAI(ctx context.Context, prompt string, cfg llmConfig) (string, error) {
	if strings.TrimSpace(cfg.OpenAIAPIKey) == "" {
		return "", fmt.Errorf("OPENAI_API_KEY is not configured")
	}
	url := strings.TrimRight(cfg.OpenAIBaseURL, "/") + "/chat/completions"
	payload := map[string]any{
		"model":       cfg.OpenAIModel,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"temperature": 0.3,
		"stream":      true,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := t.llmClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("openai http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}
		return parseOpenAIJSONContent(raw)
	}
	// Read SSE stream, concatenating delta content.
	var sb strings.Builder
	var nonSSE strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			if trimmed != "" {
				nonSSE.WriteString(trimmed)
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			sb.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		// A few OpenAI-compatible gateways forget to terminate the stream. If
		// they already sent useful content, let the XML parser decide whether it
		// is complete instead of discarding the whole batch on deadline.
		if sb.Len() > 0 {
			log.Printf("[llm] openai stream ended with %v after content; validating partial response", err)
			return sb.String(), nil
		}
		return "", fmt.Errorf("openai stream read error: %w", err)
	}
	if sb.Len() == 0 {
		if raw := strings.TrimSpace(nonSSE.String()); strings.HasPrefix(raw, "{") {
			return parseOpenAIJSONContent([]byte(raw))
		}
		return "", fmt.Errorf("openai returned empty content from stream")
	}
	return sb.String(), nil
}

func parseOpenAIJSONContent(raw []byte) (string, error) {
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("openai decode response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("openai returned no choices")
	}
	content := result.Choices[0].Message.Content
	if content == "" {
		content = result.Choices[0].Delta.Content
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("openai returned empty content")
	}
	return content, nil
}

func buildXMLInput(texts []string) string {
	var b strings.Builder
	for i, s := range texts {
		b.WriteString("<item id=\"")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("\">")
		b.WriteString(xmlEscape(s))
		b.WriteString("</item>\n")
	}
	return b.String()
}

var (
	reThink = regexp.MustCompile(`(?s)<think>.*?</think>`)
	reTrans = regexp.MustCompile(`(?s)<t\s+id="(\d+)">(.*?)</t>`)
)

func parseXMLTranslations(content string, expected int) []string {
	content = reThink.ReplaceAllString(content, "")
	out := make([]string, expected)
	for _, m := range reTrans.FindAllStringSubmatch(content, -1) {
		id, err := strconv.Atoi(m[1])
		if err != nil || id <= 0 || id > expected {
			continue
		}
		out[id-1] = xmlUnescape(strings.TrimSpace(m[2]))
	}
	return out
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func xmlUnescape(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}
