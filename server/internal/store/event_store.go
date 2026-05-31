package store

import (
	"database/sql"
	"time"

	"moesekai/server/internal/db"
	"moesekai/server/internal/model"
)

// EventStore provides CRUD over event story data backed by SQLite.
type EventStore struct {
	db *db.DB
}

func NewEventStore(database *db.DB) *EventStore {
	return &EventStore{db: database}
}

// OrderedEpisode is an episode with explicit line ordering, used for lossless
// import (map iteration order is not stable, but dialogue flow must survive).
type OrderedEpisode struct {
	EpisodeNo    string
	ScenarioID   string
	Title        string
	TitleSource  string
	TalkKeys     []string // jp keys in story order
	TalkData     map[string]string
	TalkSources  map[string]string
	SpeakerNames map[string]string
}

// ImportOrdered replaces one event story, preserving episode and line order.
func (s *EventStore) ImportOrdered(eventID int, meta model.EventStoryMeta, episodes []OrderedEpisode) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM event_stories WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	if meta.Version == "" {
		meta.Version = "1.0"
	}
	if meta.LastUpdated == 0 {
		meta.LastUpdated = time.Now().Unix()
	}
	if _, err := tx.Exec(
		`INSERT INTO event_stories (event_id, source, version, last_updated) VALUES (?, ?, ?, ?)`,
		eventID, meta.Source, meta.Version, meta.LastUpdated); err != nil {
		return err
	}

	epStmt, err := tx.Prepare(`INSERT INTO event_story_episodes
		(event_id, episode_no, scenario_id, title, title_source, talk_order_json, position)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer epStmt.Close()
	lineStmt, err := tx.Prepare(`INSERT INTO event_story_lines
		(event_id, episode_no, jp_key, cn_text, source, speaker_name, position)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer lineStmt.Close()

	for epPos, ep := range episodes {
		// talk_order_json is stored only when it adds information beyond the
		// natural line position order (kept empty here; positions drive order).
		if _, err := epStmt.Exec(eventID, ep.EpisodeNo, ep.ScenarioID, ep.Title, ep.TitleSource, "", epPos); err != nil {
			return err
		}
		for linePos, jp := range ep.TalkKeys {
			cn, ok := ep.TalkData[jp]
			if !ok {
				continue
			}
			src := ""
			if ep.TalkSources != nil {
				src = ep.TalkSources[jp]
			}
			if src == "" {
				src = meta.Source
			}
			speaker := ""
			if ep.SpeakerNames != nil {
				speaker = ep.SpeakerNames[jp]
			}
			if _, err := lineStmt.Exec(eventID, ep.EpisodeNo, jp, cn, src, speaker, linePos); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// OrderedDetail is an order-preserving read-back of one event story. Episode
// and talk-line ordering reflect stored positions (i.e. dialogue flow), which
// Go's map-based marshaling would otherwise scramble.
type OrderedDetail struct {
	Meta     model.EventStoryMeta
	Episodes []OrderedEpisode
}

// OrderedDetail loads one event story with episodes and lines in stored order.
func (s *EventStore) OrderedDetail(eventID int) (OrderedDetail, error) {
	var od OrderedDetail
	err := s.db.QueryRow(
		`SELECT source, version, last_updated FROM event_stories WHERE event_id = ?`,
		eventID).Scan(&od.Meta.Source, &od.Meta.Version, &od.Meta.LastUpdated)
	if err == sql.ErrNoRows {
		return od, sql.ErrNoRows
	}
	if err != nil {
		return od, err
	}

	epRows, err := s.db.Query(
		`SELECT episode_no, scenario_id, title, title_source
		 FROM event_story_episodes WHERE event_id = ? ORDER BY position`, eventID)
	if err != nil {
		return od, err
	}
	defer epRows.Close()
	index := map[string]int{}
	for epRows.Next() {
		var ep OrderedEpisode
		if err := epRows.Scan(&ep.EpisodeNo, &ep.ScenarioID, &ep.Title, &ep.TitleSource); err != nil {
			return od, err
		}
		ep.TalkData = map[string]string{}
		ep.TalkSources = map[string]string{}
		ep.SpeakerNames = map[string]string{}
		index[ep.EpisodeNo] = len(od.Episodes)
		od.Episodes = append(od.Episodes, ep)
	}
	if err := epRows.Err(); err != nil {
		return od, err
	}

	lineRows, err := s.db.Query(
		`SELECT episode_no, jp_key, cn_text, source, speaker_name
		 FROM event_story_lines WHERE event_id = ? ORDER BY episode_no, position`, eventID)
	if err != nil {
		return od, err
	}
	defer lineRows.Close()
	for lineRows.Next() {
		var no, jp, cn, src, speaker string
		if err := lineRows.Scan(&no, &jp, &cn, &src, &speaker); err != nil {
			return od, err
		}
		i, ok := index[no]
		if !ok {
			continue
		}
		ep := &od.Episodes[i]
		ep.TalkKeys = append(ep.TalkKeys, jp)
		ep.TalkData[jp] = cn
		ep.TalkSources[jp] = src
		if speaker != "" {
			ep.SpeakerNames[jp] = speaker
		}
	}
	return od, lineRows.Err()
}

// Detail reconstructs the full event story detail for the console API. talkOrder
// is populated from stored line positions so the editor renders dialogue in
// story order (Go map marshaling alone would sort keys alphabetically).
func (s *EventStore) Detail(eventID int) (model.EventStoryDetail, error) {
	od, err := s.OrderedDetail(eventID)
	if err != nil {
		return model.EventStoryDetail{}, err
	}
	detail := model.EventStoryDetail{
		Meta:     od.Meta,
		Episodes: make(map[string]model.EventStoryEpisode, len(od.Episodes)),
	}
	for _, ep := range od.Episodes {
		e := model.EventStoryEpisode{
			ScenarioID:   ep.ScenarioID,
			Title:        ep.Title,
			TitleSource:  ep.TitleSource,
			TalkData:     ep.TalkData,
			TalkSources:  ep.TalkSources,
			TalkOrder:    ep.TalkKeys,
			SpeakerNames: ep.SpeakerNames,
		}
		if len(e.TalkSources) == 0 {
			e.TalkSources = nil
		}
		if len(e.SpeakerNames) == 0 {
			e.SpeakerNames = nil
		}
		detail.Episodes[ep.EpisodeNo] = e
	}
	return detail, nil
}

// List returns summaries of all event stories, ordered by event id. The
// untranslated count mirrors UntranslatedTargets: talk lines with empty cn_text
// plus jp-pending titles (non-empty title text whose source is unset/unknown).
func (s *EventStore) List() ([]model.EventStorySummary, error) {
	rows, err := s.db.Query(`SELECT es.event_id, es.source, es.last_updated,
		(SELECT COUNT(*) FROM event_story_episodes e WHERE e.event_id = es.event_id),
		(SELECT COUNT(*) FROM event_story_lines l
		   WHERE l.event_id = es.event_id AND l.cn_text = '')
		+ (SELECT COUNT(*) FROM event_story_episodes e
		   WHERE e.event_id = es.event_id AND e.title <> ''
		     AND (e.title_source = '' OR e.title_source = 'unknown' OR e.title_source = 'jp_pending'))
		FROM event_stories es ORDER BY es.event_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.EventStorySummary
	for rows.Next() {
		var sum model.EventStorySummary
		if err := rows.Scan(&sum.EventID, &sum.Source, &sum.LastUpdated, &sum.EpisodeCount, &sum.UntranslatedCount); err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// UpdateLine sets the cn text and source of one talk line (entryType "talk")
// or an episode title (entryType "title"). For titles, jpKey is ignored. The
// story's last_updated is bumped. Returns ErrNoRows if the target is missing.
func (s *EventStore) UpdateLine(eventID int, episodeNo, jpKey, cnText, source, entryType string) error {
	var res sql.Result
	var err error
	if entryType == "title" {
		res, err = s.db.Exec(
			`UPDATE event_story_episodes SET title = ?, title_source = ?
			 WHERE event_id = ? AND episode_no = ?`,
			cnText, source, eventID, episodeNo)
	} else {
		res, err = s.db.Exec(
			`UPDATE event_story_lines SET cn_text = ?, source = ?
			 WHERE event_id = ? AND episode_no = ? AND jp_key = ?`,
			cnText, source, eventID, episodeNo, jpKey)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	_, _ = s.db.Exec(`UPDATE event_stories SET last_updated = ? WHERE event_id = ?`,
		time.Now().Unix(), eventID)
	return nil
}

// PromoteHuman marks every title and talk line of an event story as human.
func (s *EventStore) PromoteHuman(eventID int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE event_story_episodes SET title_source = 'human' WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE event_story_lines SET source = 'human' WHERE event_id = ?`, eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE event_stories SET source = 'human', last_updated = ? WHERE event_id = ?`,
		time.Now().Unix(), eventID); err != nil {
		return err
	}
	return tx.Commit()
}

// Exists reports whether an event story is present.
func (s *EventStore) Exists(eventID int) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM event_stories WHERE event_id = ?`, eventID).Scan(&n)
	return n > 0, err
}
