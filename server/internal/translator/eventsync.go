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
func (t *Translator) syncEventStoriesCNOnly(progressCurrent, progressTotal int) (int, int, error) {
	jpStories, err := t.fetchMasterdata("eventStories.json", "jp")
	if err != nil {
		return 0, 0, err
	}
	cnStories, err := t.fetchMasterdata("eventStories.json", "cn")
	if err != nil {
		return 0, 0, err
	}
	cnEvents, err := t.fetchMasterdata("events.json", "cn")
	if err != nil {
		return 0, 0, err
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
		return 0, 0, err
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

	processed := 0
	scenarioFailures := 0
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
		episodes, hasTalk, _, errs := t.buildOfficialCNEpisodes(jpStory, cnStoryByEvent[eventID])
		if errs > 0 {
			scenarioFailures += errs
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
			return processed, scenarioFailures, err
		}
		states[eventID] = store.EventSyncState{EventID: eventID, Source: "official_cn", IsOfficialCN: true}
		if eventID > localMax {
			localMax = eventID
		}
		processed++
	}

	if stoppedByEmpty {
		fallbackStart := localMax + 1
		log.Printf("[translate] event stories: CN empty streak at event %d, JP-pending fallback from %d", lastChecked, fallbackStart)
		fp, fetchFailures, err := t.fillEventStoriesJPPending(jpStories, fallbackStart, states, progressCurrent, progressTotal)
		if err != nil {
			return processed, scenarioFailures, err
		}
		processed += fp
		scenarioFailures += fetchFailures
	}
	return processed, scenarioFailures, nil
}

// buildOfficialCNEpisodes fetches JP + CN scenarios and pairs JP text to CN
// translation by position. Returns (episodes, hasTalkData, hasTitleOnly, errors).
func (t *Translator) buildOfficialCNEpisodes(jpStory, cnStory map[string]any) (map[string]builtEpisode, bool, bool, int) {
	asset := getString(jpStory, "assetbundleName")
	jpEpisodes := toMapSlice(jpStory["eventStoryEpisodes"])
	cnByEp := byIntID(toMapSlice(cnStory["eventStoryEpisodes"]), "episodeNo")

	episodes := map[string]builtEpisode{}
	hasTalk, hasTitleOnly, errs := false, false, 0
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
			errs++
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
func (t *Translator) buildJPPendingEpisodes(jpStory map[string]any) (map[string]builtEpisode, int) {
	asset := getString(jpStory, "assetbundleName")
	jpEpisodes := toMapSlice(jpStory["eventStoryEpisodes"])
	episodes := map[string]builtEpisode{}
	errs := 0
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
			errs++
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
func (t *Translator) fillEventStoriesJPPending(jpStories []map[string]any, startEventID int, states map[int]store.EventSyncState, progressCurrent, progressTotal int) (int, int, error) {
	processed := 0
	fetchFailures := 0
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
		episodes, errs := t.buildJPPendingEpisodes(jpStory)
		fetchFailures += errs
		if len(episodes) == 0 {
			continue
		}
		meta := model.EventStoryMeta{Source: "jp_pending", Version: "1.0", LastUpdated: time.Now().Unix()}
		if err := t.eventStore.ImportOrdered(eventID, meta, toOrderedEpisodes(episodes, "unknown")); err != nil {
			return processed, fetchFailures, err
		}
		states[eventID] = store.EventSyncState{EventID: eventID, Source: "jp_pending"}
		// Auto-translate the freshly written JP-pending story.
		if _, err := t.autoTranslateEventStory(eventID); err == nil {
			processed++
		}
	}
	return processed, fetchFailures, nil
}
