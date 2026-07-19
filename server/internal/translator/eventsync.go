package translator

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

// builtEpisode is an episode assembled from remote scenario data, with line
// order preserved (dialogue flow).
type builtEpisode struct {
	episodeNo    string
	scenarioID   string
	title        string
	talkKeys     []string
	talkData     map[string]string
	speakerNames map[string]string
}

type eventStorySyncOutcome struct {
	Processed            int
	PartialErrors        []error
	AITranslationSkipped int
	AITranslationNote    string
}

// toOrdered converts built episodes (keyed by episode no) into the ordered
// slice the EventStore import expects, sorted by numeric episode number.
func toOrderedEpisodes(eps map[string]builtEpisode, lineSource string) []store.OrderedEpisode {
	nos := make([]string, 0, len(eps))
	for no := range eps {
		nos = append(nos, no)
	}
	sort.Slice(nos, func(i, j int) bool { return atoiSafe(nos[i]) < atoiSafe(nos[j]) })
	out := make([]store.OrderedEpisode, 0, len(nos))
	for _, no := range nos {
		ep := eps[no]
		sources := make(map[string]string, len(ep.talkKeys))
		for _, jp := range ep.talkKeys {
			sources[jp] = lineSource
		}
		out = append(out, store.OrderedEpisode{
			EpisodeNo:    ep.episodeNo,
			ScenarioID:   ep.scenarioID,
			Title:        ep.title,
			TitleSource:  lineSource,
			TalkKeys:     ep.talkKeys,
			TalkData:     ep.talkData,
			TalkSources:  sources,
			SpeakerNames: ep.speakerNames,
		})
	}
	return out
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// syncEventStoriesCNOnly mirrors the legacy strategy: walk JP stories from the
// first not-yet-official event, write official CN where available, and on a
// 3-event empty streak fall back to JP-pending + auto LLM for newer events.
func (t *Translator) syncEventStoriesCNOnly(progressCurrent, progressTotal int) (eventStorySyncOutcome, error) {
	outcome := eventStorySyncOutcome{}
	jpStories, err := t.fetchMasterdata("eventStories.json", "jp")
	if err != nil {
		return outcome, err
	}
	cnStories, err := t.fetchMasterdata("eventStories.json", "cn")
	if err != nil {
		return outcome, err
	}
	cnEvents, err := t.fetchMasterdata("events.json", "cn")
	if err != nil {
		return outcome, err
	}
	cnStoryByEvent := byIntID(cnStories, "eventId")
	cnEventSet := map[int]bool{}
	for _, e := range cnEvents {
		cnEventSet[getInt(e, "id")] = true
	}
	sort.Slice(jpStories, func(i, j int) bool {
		return getInt(jpStories[i], "eventId") < getInt(jpStories[j], "eventId")
	})

	states, localMax, err := t.eventStore.EventSyncStates()
	if err != nil {
		return outcome, err
	}
	latestOfficialCN, firstLLM := 0, 0
	for _, st := range states {
		if st.IsOfficialCN && st.EventID > latestOfficialCN {
			latestOfficialCN = st.EventID
		}
		if st.IsLLM && (firstLLM == 0 || st.EventID < firstLLM) {
			firstLLM = st.EventID
		}
	}
	startCN := 1
	if firstLLM > 0 {
		startCN = firstLLM
	} else if latestOfficialCN > 0 {
		startCN = latestOfficialCN + 1
	}

	emptyStreak := 0
	stoppedByEmpty := false
	lastChecked := 0

	for _, jpStory := range jpStories {
		eventID := getInt(jpStory, "eventId")
		if eventID < startCN {
			continue
		}
		lastChecked = eventID

		if st, ok := states[eventID]; ok && (st.IsOfficialCN || st.PreserveLocal) {
			emptyStreak = 0
			continue
		}
		if !cnEventSet[eventID] || cnStoryByEvent[eventID] == nil {
			emptyStreak++
			if emptyStreak >= 3 {
				stoppedByEmpty = true
				break
			}
			continue
		}

		t.setNote(fmt.Sprintf("cn-sync event story %d", eventID))
		t.emit("sync.progress", fmt.Sprintf("正在更新活动剧情 Event #%d", eventID), progressCurrent, progressTotal)
		episodes, hasTalk, _, episodeErrors := t.buildOfficialCNEpisodes(jpStory, cnStoryByEvent[eventID])
		if len(episodeErrors) > 0 {
			for _, episodeErr := range episodeErrors {
				wrapped := fmt.Errorf("event %d: %w", eventID, episodeErr)
				outcome.PartialErrors = append(outcome.PartialErrors, wrapped)
				log.Printf("[translate] event story partial failure: %v", wrapped)
			}
			continue // scenario fetch failed; retry next round
		}
		if !hasTalk {
			emptyStreak++
			if emptyStreak >= 3 {
				stoppedByEmpty = true
				break
			}
			continue
		}
		emptyStreak = 0

		meta := model.EventStoryMeta{Source: "official_cn", Version: "1.0", LastUpdated: time.Now().Unix()}
		if err := t.eventStore.ImportOrdered(eventID, meta, toOrderedEpisodes(episodes, "cn")); err != nil {
			return outcome, err
		}
		states[eventID] = store.EventSyncState{EventID: eventID, Source: "official_cn", IsOfficialCN: true}
		if eventID > localMax {
			localMax = eventID
		}
		outcome.Processed++
	}

	if stoppedByEmpty {
		fallbackStart := localMax + 1
		log.Printf("[translate] event stories: CN empty streak at event %d, JP-pending fallback from %d", lastChecked, fallbackStart)
		fallbackOutcome, err := t.fillEventStoriesJPPending(jpStories, fallbackStart, states, progressCurrent, progressTotal)
		if err != nil {
			return outcome, err
		}
		outcome.Processed += fallbackOutcome.Processed
		outcome.PartialErrors = append(outcome.PartialErrors, fallbackOutcome.PartialErrors...)
		outcome.AITranslationSkipped += fallbackOutcome.AITranslationSkipped
		outcome.AITranslationNote = fallbackOutcome.AITranslationNote
	}
	return outcome, nil
}

// buildOfficialCNEpisodes fetches JP + CN scenarios and pairs JP text to CN
// translation by position. Returns (episodes, hasTalkData, hasTitleOnly, errors).
func (t *Translator) buildOfficialCNEpisodes(jpStory, cnStory map[string]any) (map[string]builtEpisode, bool, bool, []error) {
	asset := getString(jpStory, "assetbundleName")
	jpEpisodes := toMapSlice(jpStory["eventStoryEpisodes"])
	cnByEp := byIntID(toMapSlice(cnStory["eventStoryEpisodes"]), "episodeNo")

	episodes := map[string]builtEpisode{}
	hasTalk, hasTitleOnly := false, false
	var errs []error
	type fetchResult struct {
		ep         map[string]any
		epNo       int
		scenarioID string
		jpScenario any
		cnScenario any
		err        error
	}
	jobs := make(chan map[string]any)
	results := make(chan fetchResult, len(jpEpisodes))
	workers := t.fetchConcurrency()
	if workers > len(jpEpisodes) {
		workers = len(jpEpisodes)
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ep := range jobs {
				epNo := getInt(ep, "episodeNo")
				scenarioID := getString(ep, "scenarioId")
				if scenarioID == "" {
					continue
				}
				scenarioPath := fmt.Sprintf("event_story/%s/scenario/%s", asset, scenarioID)
				jpScenario, err := t.fetchJPScenarioJSON(scenarioPath)
				if err != nil {
					results <- fetchResult{ep: ep, epNo: epNo, scenarioID: scenarioID, err: err}
					continue
				}
				cnScenario, err := t.fetchCNScenarioJSON(scenarioPath)
				results <- fetchResult{
					ep: ep, epNo: epNo, scenarioID: scenarioID,
					jpScenario: jpScenario, cnScenario: cnScenario, err: err,
				}
			}
		}()
	}
	go func() {
		for _, ep := range jpEpisodes {
			jobs <- ep
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	for fetched := range results {
		if fetched.err != nil {
			errs = append(errs, fmt.Errorf("episode %d (%s): %w", fetched.epNo, fetched.scenarioID, fetched.err))
			continue
		}
		ep, epNo, scenarioID := fetched.ep, fetched.epNo, fetched.scenarioID
		jpScenario, cnScenario := fetched.jpScenario, fetched.cnScenario

		jpTalk := toMapSlice(asMap(jpScenario)["TalkData"])
		cnTalk := toMapSlice(asMap(cnScenario)["TalkData"])
		talkData := map[string]string{}
		speakerNames := map[string]string{}
		var talkOrder []string
		seen := map[string]bool{}
		for i := 0; i < len(jpTalk) && i < len(cnTalk); i++ {
			jpBody := strings.TrimSpace(getString(jpTalk[i], "Body"))
			cnBody := strings.TrimSpace(getString(cnTalk[i], "Body"))
			cnSpeaker := strings.TrimSpace(getString(cnTalk[i], "WindowDisplayName"))
			if jpBody != "" && cnBody != "" && jpBody != cnBody {
				talkData[jpBody] = cnBody
				if !seen[jpBody] {
					talkOrder = append(talkOrder, jpBody)
					seen[jpBody] = true
				}
				if cnSpeaker != "" {
					speakerNames[jpBody] = cnSpeaker
				}
			}
			jpName := strings.TrimSpace(getString(jpTalk[i], "WindowDisplayName"))
			cnName := strings.TrimSpace(getString(cnTalk[i], "WindowDisplayName"))
			if jpName != "" && cnName != "" && jpName != cnName {
				talkData[jpName] = cnName
				if !seen[jpName] {
					talkOrder = append(talkOrder, jpName)
					seen[jpName] = true
				}
			}
		}

		cnTitle := strings.TrimSpace(getString(cnByEp[epNo], "title"))
		if cnTitle == strings.TrimSpace(getString(ep, "title")) {
			cnTitle = ""
		}
		if len(talkData) > 0 {
			hasTalk = true
		} else if cnTitle != "" {
			hasTitleOnly = true
		}
		if len(talkData) == 0 && cnTitle == "" {
			continue
		}
		episodes[strconv.Itoa(epNo)] = builtEpisode{
			episodeNo:    strconv.Itoa(epNo),
			scenarioID:   scenarioID,
			title:        cnTitle,
			talkKeys:     talkOrder,
			talkData:     talkData,
			speakerNames: speakerNames,
		}
	}
	return episodes, hasTalk, hasTitleOnly, errs
}

// buildJPPendingEpisodes fetches JP-only scenario text (no CN), leaving cn empty.
func (t *Translator) buildJPPendingEpisodes(jpStory map[string]any) (map[string]builtEpisode, []error) {
	asset := getString(jpStory, "assetbundleName")
	jpEpisodes := toMapSlice(jpStory["eventStoryEpisodes"])
	episodes := map[string]builtEpisode{}
	var errs []error
	type fetchResult struct {
		ep         map[string]any
		epNo       int
		scenarioID string
		jpScenario any
		err        error
	}
	jobs := make(chan map[string]any)
	results := make(chan fetchResult, len(jpEpisodes))
	workers := t.fetchConcurrency()
	if workers > len(jpEpisodes) {
		workers = len(jpEpisodes)
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ep := range jobs {
				epNo := getInt(ep, "episodeNo")
				scenarioID := getString(ep, "scenarioId")
				if scenarioID == "" {
					continue
				}
				scenarioPath := fmt.Sprintf("event_story/%s/scenario/%s", asset, scenarioID)
				jpScenario, err := t.fetchJPScenarioJSON(scenarioPath)
				results <- fetchResult{
					ep: ep, epNo: epNo, scenarioID: scenarioID,
					jpScenario: jpScenario, err: err,
				}
			}
		}()
	}
	go func() {
		for _, ep := range jpEpisodes {
			jobs <- ep
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	for fetched := range results {
		ep, epNo, scenarioID := fetched.ep, fetched.epNo, fetched.scenarioID
		title := strings.TrimSpace(getString(ep, "title"))
		if fetched.err != nil {
			errs = append(errs, fmt.Errorf("episode %d (%s): %w", epNo, scenarioID, fetched.err))
			if title != "" {
				episodes[strconv.Itoa(epNo)] = builtEpisode{
					episodeNo: strconv.Itoa(epNo), scenarioID: scenarioID,
					title: title, talkData: map[string]string{},
				}
			}
			continue
		}
		jpScenario := fetched.jpScenario
		jpTalk := toMapSlice(asMap(jpScenario)["TalkData"])
		talkData := map[string]string{}
		speakerNames := map[string]string{}
		var talkOrder []string
		seen := map[string]bool{}
		for _, talk := range jpTalk {
			jpBody := strings.TrimSpace(getString(talk, "Body"))
			jpSpeaker := strings.TrimSpace(getString(talk, "WindowDisplayName"))
			if jpBody != "" {
				talkData[jpBody] = ""
				if !seen[jpBody] {
					talkOrder = append(talkOrder, jpBody)
					seen[jpBody] = true
				}
				if jpSpeaker != "" {
					speakerNames[jpBody] = jpSpeaker
				}
			}
			if jpSpeaker != "" {
				talkData[jpSpeaker] = ""
				if !seen[jpSpeaker] {
					talkOrder = append(talkOrder, jpSpeaker)
					seen[jpSpeaker] = true
				}
			}
		}
		if len(talkData) == 0 && title == "" {
			continue
		}
		episodes[strconv.Itoa(epNo)] = builtEpisode{
			episodeNo: strconv.Itoa(epNo), scenarioID: scenarioID,
			title: title, talkKeys: talkOrder, talkData: talkData, speakerNames: speakerNames,
		}
	}
	return episodes, errs
}

// fillEventStoriesJPPending writes JP-pending stories for new events and runs
// auto LLM translation on them.
func (t *Translator) fillEventStoriesJPPending(jpStories []map[string]any, startEventID int, states map[int]store.EventSyncState, progressCurrent, progressTotal int) (eventStorySyncOutcome, error) {
	outcome := eventStorySyncOutcome{}
	skipAI := false
	if reason, unavailable := t.automaticLLMUnavailable(); unavailable {
		skipAI = true
		outcome.AITranslationNote = reason
	}
	for _, jpStory := range jpStories {
		eventID := getInt(jpStory, "eventId")
		if eventID < startEventID {
			continue
		}
		if _, exists := states[eventID]; exists {
			continue
		}
		t.setNote(fmt.Sprintf("cn-sync JP-pending event story %d", eventID))
		t.emit("sync.progress", fmt.Sprintf("正在拉取 JP 剧情 Event #%d", eventID), progressCurrent, progressTotal)
		episodes, episodeErrors := t.buildJPPendingEpisodes(jpStory)
		for _, episodeErr := range episodeErrors {
			wrapped := fmt.Errorf("event %d: %w", eventID, episodeErr)
			outcome.PartialErrors = append(outcome.PartialErrors, wrapped)
			log.Printf("[translate] event story partial failure: %v", wrapped)
		}
		if len(episodes) == 0 {
			continue
		}
		meta := model.EventStoryMeta{Source: "jp_pending", Version: "1.0", LastUpdated: time.Now().Unix()}
		if err := t.eventStore.ImportOrdered(eventID, meta, toOrderedEpisodes(episodes, "unknown")); err != nil {
			return outcome, err
		}
		states[eventID] = store.EventSyncState{EventID: eventID, Source: "jp_pending"}
		outcome.Processed++
		if skipAI {
			outcome.AITranslationSkipped++
			continue
		}
		// Auto-translate the freshly written JP-pending story. Failure is
		// optional: it does not discard imported data or fail the CN sync. Once
		// unavailable, skip AI for the rest of this run and leave JP pending.
		if translated, err := t.autoTranslateEventStory(eventID); err != nil {
			skipAI = true
			outcome.AITranslationSkipped++
			outcome.AITranslationNote = fmt.Sprintf("Event #%d 自动 AI 翻译在保存 %d 条后暂停：%v", eventID, translated, err)
			log.Printf("[translate] %s", outcome.AITranslationNote)
			t.emit("sync.progress", fmt.Sprintf("Event #%d 已保存，LLM 不可用，已跳过后续 AI", eventID), progressCurrent, progressTotal)
		}
	}
	return outcome, nil
}
