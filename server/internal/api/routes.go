package api

import "net/http"

// RegisterRoutes mounts all console API routes on mux. Every route except login
// requires a valid JWT; admin-only routes (registered elsewhere) additionally
// require the admin role. Public, cacheable file serving is mounted separately.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// Auth
	mux.HandleFunc("/api/auth/setup-status", s.handleSetupStatus)
	mux.HandleFunc("/api/auth/setup", s.handleSetup)
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/me", s.auth.RequireAuth(s.handleMe))
	mux.HandleFunc("/api/auth/refresh", s.auth.RequireAuth(s.handleRefresh))

	// Translations
	mux.HandleFunc("/api/categories", s.auth.RequireAuth(s.handleCategories))
	mux.HandleFunc("/api/entries", s.auth.RequireAuth(s.handleEntries))
	mux.HandleFunc("/api/entry", s.auth.RequireAuth(s.handleUpdateEntry))

	// Event stories
	mux.HandleFunc("/api/event-stories", s.auth.RequireAuth(s.handleEventStories))
	mux.HandleFunc("/api/event-story", s.auth.RequireAuth(s.handleEventStory))
	mux.HandleFunc("/api/event-story/update", s.auth.RequireAuth(s.handleUpdateEventStory))
	mux.HandleFunc("/api/event-story/promote-human", s.auth.RequireAuth(s.handlePromoteEventStoryHuman))
	mux.HandleFunc("/api/event-story/retry", s.auth.RequireAuth(s.handleRetryEventStory))
	mux.HandleFunc("/api/event-story/reorder", s.auth.RequireAuth(s.handleReorderEventStory))

	// Translation engine
	mux.HandleFunc("/api/translate/status", s.auth.RequireAuth(s.handleTranslateStatus))
	mux.HandleFunc("/api/translate/cn-sync", s.auth.RequireAuth(s.handleCNSync))
	mux.HandleFunc("/api/translate/ai", s.auth.RequireAuth(s.handleTranslateAI))
	mux.HandleFunc("/api/translate/ai-all", s.auth.RequireAuth(s.handleTranslateAIAll))
	mux.HandleFunc("/api/translate/ai-story", s.auth.RequireAuth(s.handleTranslateAIStory))

	// Read-only upstream status for any authenticated user (user settings page).
	mux.HandleFunc("/api/upstream/status", s.auth.RequireAuth(s.handleUpstreamStatus))

	// Admin (admin role required)
	mux.HandleFunc("/api/admin/users", s.auth.RequireAdmin(s.handleUsersRouter))
	mux.HandleFunc("/api/admin/settings", s.auth.RequireAdmin(s.handleSettingsRouter))
	mux.HandleFunc("/api/admin/upstream", s.auth.RequireAdmin(s.handleUpstreamStatus))
	mux.HandleFunc("/api/admin/upstream/check", s.auth.RequireAdmin(s.handleUpstreamCheck))

	// Backup / restore (admin role required)
	mux.HandleFunc("/api/backup/status", s.auth.RequireAdmin(s.handleBackupStatus))
	mux.HandleFunc("/api/backup/push", s.auth.RequireAdmin(s.handleBackupPush))
	mux.HandleFunc("/api/backup/restore", s.auth.RequireAdmin(s.handleBackupRestore))

	// Realtime: SSE stream (JWT via Authorization header or ?token= query param).
	if s.hub != nil {
		mux.HandleFunc("/sse", s.auth.RequireAuth(s.hub.Handler(currentUser)))
	}
}
