package translator

import (
	"bufio"
	"bytes"
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

// callLLM translates a batch of JP texts via the given provider, retrying up to
// 3 times. Returns a slice aligned to texts (empty string where unparsed).
func (t *Translator) callLLM(provider string, texts []string) ([]string, error) {
	if len(texts) == 0 {
		return []string{}, nil
	}
	prompt := gameContextPrompt + buildXMLInput(texts)
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		var content string
		var err error
		switch provider {
		case "gemini":
			content, err = t.callGemini(prompt)
		case "openai":
			content, err = t.callOpenAI(prompt)
		default:
			return nil, fmt.Errorf("unsupported provider: %s", provider)
		}
		if err != nil {
			lastErr = err
			log.Printf("[llm] %s attempt %d/3 request error: %v", provider, attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		parsed := parseXMLTranslations(content, len(texts))
		nonEmpty := 0
		for _, s := range parsed {
			if strings.TrimSpace(s) != "" {
				nonEmpty++
			}
		}
		if len(parsed) == len(texts) && nonEmpty >= len(texts)/2 {
			return parsed, nil
		}
		if nonEmpty >= len(texts)/2 {
			return parsed, nil
		}
		lastErr = fmt.Errorf("parse incomplete: %d non-empty of %d", nonEmpty, len(texts))
		log.Printf("[llm] %s attempt %d/3 %v", provider, attempt, lastErr)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	log.Printf("[llm] %s gave up after 3 retries (texts=%d): %v", provider, len(texts), lastErr)
	return nil, fmt.Errorf("llm failed after 3 retries (provider=%s, texts=%d): %v", provider, len(texts), lastErr)
}

func (t *Translator) callGemini(prompt string) (string, error) {
	cfg := t.snapshotConfig()
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
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", cfg.GeminiAPIKey)
	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini http %d: %s", resp.StatusCode, string(raw))
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

func (t *Translator) callOpenAI(prompt string) (string, error) {
	cfg := t.snapshotConfig()
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
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.OpenAIAPIKey)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openai http %d: %s", resp.StatusCode, string(raw))
	}
	// Read SSE stream, concatenating delta content.
	var sb strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
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
		return "", fmt.Errorf("openai stream read error: %w", err)
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("openai returned empty content from stream")
	}
	return sb.String(), nil
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
