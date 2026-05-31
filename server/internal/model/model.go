package model

// Source priority: pinned > human > cn > llm > unknown
const (
	SourceCN      = "cn"
	SourceHuman   = "human"
	SourcePinned  = "pinned"
	SourceLLM     = "llm"
	SourceUnknown = "unknown"
)

// SupportedCategories are the 11 flat translation categories (event stories
// are handled separately). Order is preserved for stable category listing.
var SupportedCategories = []string{
	"cards", "events", "music", "gacha", "virtualLive",
	"sticker", "comic", "mysekai", "costumes", "characters", "units",
}

func IsValidCategory(category string) bool {
	for _, c := range SupportedCategories {
		if c == category {
			return true
		}
	}
	return false
}

// Entry is a single translation row in the .full.json format:
//
//	{ "text": ..., "source": ..., "ids": [...] }
type Entry struct {
	Text   string   `json:"text"`
	Source string   `json:"source"`
	Ids    []string `json:"ids,omitempty"`
}

// Category is field -> { jpKey -> Entry }, matching X.full.json on disk.
type Category map[string]map[string]Entry

// EntryWithKey is an entry returned to the console API with its jp key.
type EntryWithKey struct {
	Key    string   `json:"key"`
	Text   string   `json:"text"`
	Source string   `json:"source"`
	Ids    []string `json:"ids,omitempty"`
}

// FieldInfo holds per-field counts for the sidebar.
type FieldInfo struct {
	Name         string `json:"name"`
	Total        int    `json:"total"`
	CnCount      int    `json:"cnCount"`
	HumanCount   int    `json:"humanCount"`
	PinnedCount  int    `json:"pinnedCount"`
	LlmCount     int    `json:"llmCount"`
	UnknownCount int    `json:"unknownCount"`
}

type CategoryInfo struct {
	Name   string      `json:"name"`
	Fields []FieldInfo `json:"fields"`
}

// ---- Event story formats (compatible with eventStory/event_N.json) ----

type EventStoryMeta struct {
	Source      string `json:"source"`
	Version     string `json:"version"`
	LastUpdated int64  `json:"last_updated"`
}

type EventStoryEpisode struct {
	ScenarioID   string            `json:"scenarioId"`
	Title        string            `json:"title"`
	TitleSource  string            `json:"titleSource,omitempty"`
	TalkData     map[string]string `json:"talkData"`
	TalkSources  map[string]string `json:"talkSources,omitempty"`
	TalkOrder    []string          `json:"talkOrder,omitempty"`
	SpeakerNames map[string]string `json:"speakerNames,omitempty"`
}

type EventStoryDetail struct {
	Meta     EventStoryMeta               `json:"meta"`
	Episodes map[string]EventStoryEpisode `json:"episodes"`
}

type EventStorySummary struct {
	EventID           int    `json:"eventId"`
	Source            string `json:"source"`
	EpisodeCount      int    `json:"episodeCount"`
	UntranslatedCount int    `json:"untranslatedCount"`
	LastUpdated       int64  `json:"lastUpdated"`
}
