package translator

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// AITranslateAllResult summarizes a bulk event-story AI translation run.
type AITranslateAllResult struct {
	Provider        string `json:"provider"`
	TotalFields     int    `json:"totalFields"`
	TotalCandidates int    `json:"totalCandidates"`
	TotalTranslated int    `json:"totalTranslated"`
	TotalSkipped    int    `json:"totalSkipped"`
	Errors          int    `json:"errors"`
}

// autoTranslateEventStory fills untranslated lines/titles of one event story via
// the configured provider. Returns the number of lines translated. The story
// source becomes "llm" if fully translated, else stays "jp_pending".
func (t *Translator) autoTranslateEventStory(eventID int) (int, error) {
	provider := normalizeProvider("", t.cfg.GetOr("llm.type", "openai"))
	if provider != "gemini" && provider != "openai" {
		return 0, fmt.Errorf("unsupported provider: %s", provider)
	}
	return t.translateEventStory(eventID, provider)
}

// translateEventStory runs LLM translation over an event story's untranslated
// targets and writes results, updating the story source accordingly.
func (t *Translator) translateEventStory(eventID int, provider string) (int, error) {
	targets, err := t.eventStore.UntranslatedTargets(eventID)
	if err != nil {
		return 0, err
	}
	if len(targets) == 0 {
		return 0, nil
	}
	texts := make([]string, len(targets))
	for i, tgt := range targets {
		texts[i] = tgt.JP
	}
	translated, err := t.translateOrdered(provider, texts)
	if err != nil {
		return 0, err
	}
	count, err := t.eventStore.ApplyEventTranslations(eventID, targets, translated, model.SourceLLM)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	source := "jp_pending"
	if count >= len(targets) {
		source = model.SourceLLM
	}
	if err := t.eventStore.SetStorySource(eventID, source); err != nil {
		return count, err
	}
	t.store.NotifyChange()
	return count, nil
}

// translateOrdered translates texts in batches and returns a slice aligned to
// the input (empty string where the model returned nothing).
func (t *Translator) translateOrdered(provider string, texts []string) ([]string, error) {
	cfg := t.snapshotConfig()
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}
	out := make([]string, len(texts))
	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		t.emit("translate.progress", fmt.Sprintf("AI 剧情翻译 %d/%d", end, len(texts)), end, len(texts))
		res, err := t.callLLM(provider, texts[i:end])
		if err != nil {
			return out, err
		}
		for j, cn := range res {
			if i+j < len(out) {
				out[i+j] = strings.TrimSpace(cn)
			}
		}
		if end < len(texts) {
			time.Sleep(cfg.RateDelay)
		}
	}
	return out, nil
}

// AITranslateAll translates every event story that still has untranslated lines.
func (t *Translator) AITranslateAll(provider string) (AITranslateAllResult, error) {
	if err := t.markStart("ai-all"); err != nil {
		return AITranslateAllResult{}, err
	}
	var runErr error
	defer func() { t.markEnd("ai-all complete", runErr) }()

	provider = normalizeProvider(provider, t.cfg.GetOr("llm.type", "openai"))
	result := AITranslateAllResult{Provider: provider}
	if provider != "gemini" && provider != "openai" {
		runErr = fmt.Errorf("unsupported provider: %s", provider)
		return result, runErr
	}

	summaries, err := t.eventStore.List()
	if err != nil {
		runErr = err
		return result, runErr
	}
	for _, sum := range summaries {
		// Skip stories already official/human.
		src := normalizeStorySource(sum.Source)
		if src == "official_cn" || src == model.SourceHuman || src == model.SourcePinned {
			continue
		}
		targets, err := t.eventStore.UntranslatedTargets(sum.EventID)
		if err != nil {
			result.Errors++
			continue
		}
		if len(targets) == 0 {
			continue
		}
		result.TotalFields++
		result.TotalCandidates += len(targets)
		count, err := t.translateEventStory(sum.EventID, provider)
		if err != nil {
			result.Errors++
			continue
		}
		result.TotalTranslated += count
		result.TotalSkipped += len(targets) - count
	}
	return result, nil
}

// AITranslateStory translates a single event story's untranslated lines via the
// LLM. Used by the per-story "AI 补充翻译" button in the editor.
func (t *Translator) AITranslateStory(eventID int, provider string) (AITranslateAllResult, error) {
	if err := t.markStart("ai-story"); err != nil {
		return AITranslateAllResult{}, err
	}
	var runErr error
	defer func() { t.markEnd(fmt.Sprintf("ai story %d complete", eventID), runErr) }()

	provider = normalizeProvider(provider, t.cfg.GetOr("llm.type", "openai"))
	result := AITranslateAllResult{Provider: provider}
	if provider != "gemini" && provider != "openai" {
		runErr = fmt.Errorf("unsupported provider: %s", provider)
		return result, runErr
	}

	targets, err := t.eventStore.UntranslatedTargets(eventID)
	if err != nil {
		runErr = err
		return result, runErr
	}
	if len(targets) == 0 {
		return result, nil
	}
	result.TotalFields = 1
	result.TotalCandidates = len(targets)
	count, err := t.translateEventStory(eventID, provider)
	if err != nil {
		runErr = err
		result.Errors++
		return result, runErr
	}
	result.TotalTranslated = count
	result.TotalSkipped = len(targets) - count
	return result, nil
}

// RetryEventStorySync re-fetches one event story from remote, preferring official
// CN and falling back to JP-pending + auto LLM. Overwrites local non-edited data.
func (t *Translator) RetryEventStorySync(eventID int) (map[string]any, error) {
	if err := t.markStart("retry-event-story"); err != nil {
		return nil, err
	}
	var runErr error
	defer func() { t.markEnd(fmt.Sprintf("retry event %d", eventID), runErr) }()

	jpStories, err := t.fetchMasterdata("eventStories.json", "jp")
	if err != nil {
		runErr = err
		return nil, fmt.Errorf("fetch JP eventStories: %w", err)
	}
	cnStories, err := t.fetchMasterdata("eventStories.json", "cn")
	if err != nil {
		runErr = err
		return nil, fmt.Errorf("fetch CN eventStories: %w", err)
	}
	cnEvents, err := t.fetchMasterdata("events.json", "cn")
	if err != nil {
		runErr = err
		return nil, fmt.Errorf("fetch CN events: %w", err)
	}

	var jpStory map[string]any
	for _, s := range jpStories {
		if getInt(s, "eventId") == eventID {
			jpStory = s
			break
		}
	}
	if jpStory == nil {
		runErr = fmt.Errorf("event %d not found in JP eventStories", eventID)
		return nil, runErr
	}
	cnStoryByEvent := byIntID(cnStories, "eventId")
	cnEventSet := map[int]bool{}
	for _, e := range cnEvents {
		cnEventSet[getInt(e, "id")] = true
	}

	result := map[string]any{"eventId": eventID}

	if cnEventSet[eventID] && cnStoryByEvent[eventID] != nil {
		episodes, hasTalk, _, errs := t.buildOfficialCNEpisodes(jpStory, cnStoryByEvent[eventID])
		if errs == 0 && hasTalk {
			meta := model.EventStoryMeta{Source: "official_cn", Version: "1.0", LastUpdated: time.Now().Unix()}
			if err := t.eventStore.ImportOrdered(eventID, meta, toOrderedEpisodes(episodes, "cn")); err != nil {
				runErr = err
				return nil, err
			}
			t.store.NotifyChange()
			result["source"] = "official_cn"
			result["episodes"] = len(episodes)
			return result, nil
		}
	}

	episodes, errs := t.buildJPPendingEpisodes(jpStory)
	if len(episodes) == 0 {
		runErr = fmt.Errorf("event %d: no episodes fetched (errors=%d)", eventID, errs)
		return nil, runErr
	}
	meta := model.EventStoryMeta{Source: "jp_pending", Version: "1.0", LastUpdated: time.Now().Unix()}
	if err := t.eventStore.ImportOrdered(eventID, meta, toOrderedEpisodes(episodes, "unknown")); err != nil {
		runErr = err
		return nil, err
	}
	t.store.NotifyChange()
	result["source"] = "jp_pending"
	result["episodes"] = len(episodes)
	result["fetchErrors"] = errs

	translated, autoErr := t.autoTranslateEventStory(eventID)
	if autoErr != nil {
		result["translateError"] = autoErr.Error()
	} else if translated > 0 {
		result["source"] = "llm"
		result["translated"] = translated
	}
	return result, nil
}

// ReorderEventStory re-fetches remote scenarios to obtain the original dialogue
// order and updates stored line positions, without touching translations.
func (t *Translator) ReorderEventStory(eventID int) (map[string]any, error) {
	if err := t.markStart("reorder-event-story"); err != nil {
		return nil, err
	}
	var runErr error
	defer func() { t.markEnd(fmt.Sprintf("reorder event %d", eventID), runErr) }()

	if ok, err := t.eventStore.Exists(eventID); err != nil {
		runErr = err
		return nil, err
	} else if !ok {
		runErr = sql.ErrNoRows
		return nil, fmt.Errorf("event %d not found", eventID)
	}

	jpStories, err := t.fetchMasterdata("eventStories.json", "jp")
	if err != nil {
		runErr = err
		return nil, fmt.Errorf("fetch JP eventStories: %w", err)
	}
	var jpStory map[string]any
	for _, s := range jpStories {
		if getInt(s, "eventId") == eventID {
			jpStory = s
			break
		}
	}
	if jpStory == nil {
		runErr = fmt.Errorf("event %d not found in JP eventStories", eventID)
		return nil, runErr
	}

	asset := getString(jpStory, "assetbundleName")
	reordered, fetchErrors := 0, 0
	for _, ep := range toMapSlice(jpStory["eventStoryEpisodes"]) {
		epNo := strconv.Itoa(getInt(ep, "episodeNo"))
		scenarioID := getString(ep, "scenarioId")
		if scenarioID == "" {
			continue
		}
		localKeys, err := t.eventStore.EpisodeTalkKeys(eventID, epNo)
		if err != nil || len(localKeys) == 0 {
			continue
		}
		scenarioPath := fmt.Sprintf("event_story/%s/scenario/%s", asset, scenarioID)
		jpScenario, fetchErr := t.fetchJPScenarioJSON(scenarioPath)
		if fetchErr != nil {
			fetchErrors++
			continue
		}
		var order []string
		seen := map[string]bool{}
		for _, talk := range toMapSlice(asMap(jpScenario)["TalkData"]) {
			jpBody := strings.TrimSpace(getString(talk, "Body"))
			if jpBody != "" && localKeys[jpBody] && !seen[jpBody] {
				order = append(order, jpBody)
				seen[jpBody] = true
			}
			jpName := strings.TrimSpace(getString(talk, "WindowDisplayName"))
			if jpName != "" && localKeys[jpName] && !seen[jpName] {
				order = append(order, jpName)
				seen[jpName] = true
			}
		}
		if err := t.eventStore.ReorderEpisodeLines(eventID, epNo, order); err != nil {
			runErr = err
			return nil, err
		}
		reordered++
	}
	t.store.NotifyChange()
	return map[string]any{"status": "ok", "episodes": reordered, "fetchErrors": fetchErrors}, nil
}

func normalizeStorySource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "official_cn", "official_cn_legacy", "cn":
		return "official_cn"
	case "llm":
		return model.SourceLLM
	case "human":
		return model.SourceHuman
	case "pinned":
		return model.SourcePinned
	default:
		return "jp_pending"
	}
}

var _ = store.EventTranslateTarget{}
