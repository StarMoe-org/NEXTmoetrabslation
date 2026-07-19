package api

import (
	"net/http"

	"moesekai/server/internal/sse"
	"moesekai/server/internal/translator"
)

// handleTranslateStatus reports translator + connected-client state.
//
// GET /api/translate/status
func (s *Server) handleTranslateStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"translator": s.translator.Status()}
	if s.hub != nil {
		resp["clients"] = s.hub.ClientCount()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCNSync triggers a full CN data sync. The work runs synchronously (the
// translator's single-run lock prevents overlap) and progress streams via SSE.
//
// POST /api/translate/cn-sync
func (s *Server) handleCNSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	result, err := s.translator.SyncCNOnly()
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if s.upstream != nil {
			s.upstream.RecordSyncResult(err)
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.upstream != nil {
		s.upstream.RecordSyncResult(result.SkippedError())
	}
	writeJSON(w, http.StatusOK, result)
}

// handleTranslateAI fills one field's empty entries via the LLM.
//
// POST /api/translate/ai {category, field, provider, limit}
func (s *Server) handleTranslateAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req translator.AITranslateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	result, err := s.translator.ManualAITranslate(req)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleTranslateAIAll fills every event story's untranslated lines via the LLM.
//
// POST /api/translate/ai-all {provider}
func (s *Server) handleTranslateAIAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	result, err := s.translator.AITranslateAll(req.Provider)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleTranslateAIStory fills one event story's untranslated lines via the LLM.
//
// POST /api/translate/ai-story {eventId, provider}
func (s *Server) handleTranslateAIStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		EventID  int    `json:"eventId"`
		Provider string `json:"provider"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.EventID <= 0 {
		writeErr(w, http.StatusBadRequest, "eventId required")
		return
	}
	result, err := s.translator.AITranslateStory(req.EventID, req.Provider)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcast(sse.EventStoryUpdated, map[string]any{"eventId": req.EventID, "action": "ai-translate"})
	writeJSON(w, http.StatusOK, result)
}

// handleRetryEventStory re-fetches one event story from remote.
//
// POST /api/event-story/retry {eventId}
func (s *Server) handleRetryEventStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := decodeEventID(w, r)
	if !ok {
		return
	}
	result, err := s.translator.RetryEventStorySync(id)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcast(sse.EventStoryUpdated, map[string]any{"eventId": id, "action": "retry"})
	writeJSON(w, http.StatusOK, result)
}

// handleReorderEventStory re-fetches remote dialogue order for one event story.
//
// POST /api/event-story/reorder {eventId}
func (s *Server) handleReorderEventStory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := decodeEventID(w, r)
	if !ok {
		return
	}
	result, err := s.translator.ReorderEventStory(id)
	if err != nil {
		if translator.IsAlreadyRunning(err) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.broadcast(sse.EventStoryUpdated, map[string]any{"eventId": id, "action": "reorder"})
	writeJSON(w, http.StatusOK, result)
}
