package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/api"
	"moesekai/server/internal/auth"
	"moesekai/server/internal/backup"
	"moesekai/server/internal/config"
	"moesekai/server/internal/db"
	"moesekai/server/internal/files"
	"moesekai/server/internal/filesvc"
	"moesekai/server/internal/searchindex"
	"moesekai/server/internal/sse"
	"moesekai/server/internal/store"
	"moesekai/server/internal/translator"
	"moesekai/server/internal/upstream"
)

func main() {
	// Timestamped logs (UTC) on stdout so `docker logs` shows operational activity.
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("")

	port := envOr("PORT", "9090")
	dbPath := envOr("DB_PATH", "./data/moesekai.db")
	dataDir := envOr("DATA_DIR", "./data")
	masterKey := os.Getenv("MOESEKAI_MASTER_KEY")
	jwtSecret := envOr("JWT_SECRET", "")
	allowOrigin := envOr("CONSOLE_ORIGIN", "*")

	if jwtSecret == "" {
		fmt.Fprintln(os.Stderr, "Fatal: JWT_SECRET is required")
		os.Exit(1)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		fatal("open db", err)
	}
	defer database.Close()

	cfg, err := config.New(database, masterKey)
	if err != nil {
		fatal("init config", err)
	}
	seedConfigFromEnv(cfg)

	authSvc := auth.New(database, jwtSecret, parseTTL(envOr("TOKEN_TTL_HOURS", "168")))
	seedAdminFromEnv(authSvc)

	st := store.New(database)
	es := store.NewEventStore(database)
	gen := files.NewGenerator(st, es, dataDir)

	fileService := filesvc.New(st, es, gen)
	fileService.Start()
	// Regenerate public files whenever the DB changes (debounced inside).
	st.OnChange(fileService.Trigger)

	idx := searchindex.New(st, fileService,
		parseDurMs(envOr("SEARCH_INDEX_DEBOUNCE_MS", "3600000")),
		parseDurMs(envOr("SEARCH_INDEX_REFRESH_MS", "3600000")))
	idx.Start()
	st.OnChange(idx.Trigger)

	hub := sse.NewHub()

	tr := translator.New(st, es, cfg)
	tr.SetProgress(func(stage, detail string, cur, total int) {
		hub.Broadcast(stage, map[string]any{"detail": detail, "current": cur, "total": total})
	})

	// Upstream watcher: polls current_version.json (no GitHub API rate limit),
	// optionally keeps a local masterdata git mirror, triggers CN sync on change.
	useGit := envOr("UPSTREAM_USE_GIT", "false") == "true"
	watcher := upstream.New(cfg, func() error {
		_, err := tr.SyncCNOnly()
		return err
	}, upstream.Options{
		Interval: parseDurMs(envOr("UPSTREAM_POLL_MS", "300000")),
		GitDir:   filepath.Join(dataDir, "masterdata-mirror"),
		UseGit:   useGit,
	})
	watcher.Start()

	// Backup manager: daily + manual backup/restore to S3 and/or GitHub.
	backupMgr := backup.NewManager(cfg, gen, st, es, filepath.Join(dataDir, "backup-work"))
	backupMgr.StartScheduler()

	apiServer := api.NewServer(st, es, authSvc, cfg, hub, tr, watcher, backupMgr)

	mux := http.NewServeMux()
	apiServer.RegisterRoutes(mux)
	mux.Handle("/files/", fileService.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	handler := corsMiddleware(mux, allowOrigin)
	handler = loggingMiddleware(handler)

	log.Printf("moesekai server starting on :%s", port)
	log.Printf("  db:        %s", dbPath)
	log.Printf("  data dir:  %s", dataDir)
	log.Printf("  files:     /files/* (public, cacheable)")
	log.Printf("  api:       /api/*   (JWT, no-store)")
	if !cfg.HasMasterKey() {
		log.Println("  WARNING: MOESEKAI_MASTER_KEY not set — secrets cannot be stored")
	}
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		fatal("listen", err)
	}
}

// seedConfigFromEnv writes settings from env vars on first run only, leaving the
// admin UI authoritative thereafter.
func seedConfigFromEnv(cfg *config.Config) {
	seed := map[string]string{
		config.KeyLLMType:           os.Getenv("LLM_TYPE"),
		config.KeyGeminiAPIKey:      os.Getenv("GEMINI_API_KEY"),
		config.KeyGeminiModel:       os.Getenv("GEMINI_MODEL"),
		config.KeyOpenAIAPIKey:      os.Getenv("OPENAI_API_KEY"),
		config.KeyOpenAIBaseURL:     os.Getenv("OPENAI_BASE_URL"),
		config.KeyOpenAIModel:       os.Getenv("OPENAI_MODEL"),
		config.KeyUpstreamRepo:      envOr("UPSTREAM_REPO", "Team-Haruki/haruki-sekai-master"),
		config.KeyUpstreamBranch:    envOr("UPSTREAM_BRANCH", "main"),
		config.KeySchedulerOn:       envOr("TRANSLATE_SCHEDULER_ENABLED", "true"),
		config.KeyBackupGitRepoURL:  os.Getenv("GIT_PUSH_REPO_URL"),
		config.KeyBackupGitBranch:   envOr("GIT_PUSH_BRANCH", "backup-translations"),
		config.KeyBackupS3Bucket:    os.Getenv("BACKUP_S3_BUCKET"),
		config.KeyBackupS3Region:    os.Getenv("BACKUP_S3_REGION"),
		config.KeyBackupS3Endpoint:  os.Getenv("BACKUP_S3_ENDPOINT"),
		config.KeyBackupS3AccessKey: os.Getenv("BACKUP_S3_ACCESS_KEY"),
		config.KeyBackupS3SecretKey: os.Getenv("BACKUP_S3_SECRET_KEY"),
	}
	seeded := 0
	for k, v := range seed {
		if v == "" {
			continue
		}
		// Secrets need a master key; skip silently if unavailable.
		if config.IsSecret(k) && !cfg.HasMasterKey() {
			continue
		}
		if ok, err := cfg.SetIfAbsent(k, v); err == nil && ok {
			seeded++
		}
	}
	if seeded > 0 {
		log.Printf("[config] seeded %d settings from environment", seeded)
	}
}

// seedAdminFromEnv creates the first admin from TRANSLATOR_ACCOUNTS (legacy
// "user:pass,user2:pass2") or ADMIN_USER/ADMIN_PASSWORD when no users exist.
func seedAdminFromEnv(a *auth.Auth) {
	n, err := a.CountUsers()
	if err != nil || n > 0 {
		return
	}
	created := 0
	if accts := os.Getenv("TRANSLATOR_ACCOUNTS"); accts != "" {
		for i, pair := range strings.Split(accts, ",") {
			parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
			if len(parts) != 2 {
				continue
			}
			role := auth.RoleEditor
			if i == 0 {
				role = auth.RoleAdmin // first account is admin
			}
			if _, err := a.CreateUser(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), role); err == nil {
				created++
			}
		}
	}
	if created == 0 {
		user := envOr("ADMIN_USER", "admin")
		pass := envOr("ADMIN_PASSWORD", "")
		if pass != "" {
			if _, err := a.CreateUser(user, pass, auth.RoleAdmin); err == nil {
				created++
				log.Printf("[auth] created initial admin %q from env", user)
			}
		}
	}
	if created > 0 {
		log.Printf("[auth] seeded %d account(s) from environment", created)
	}
}

func corsMiddleware(next http.Handler, origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /files/* sets its own permissive CORS; here we scope console API.
		if !strings.HasPrefix(r.URL.Path, "/files/") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseTTL(hours string) time.Duration {
	n, err := strconv.Atoi(hours)
	if err != nil || n <= 0 {
		n = 168
	}
	return time.Duration(n) * time.Hour
}

func parseDurMs(ms string) time.Duration {
	n, err := strconv.Atoi(ms)
	if err != nil || n <= 0 {
		n = 3600000
	}
	return time.Duration(n) * time.Millisecond
}

func fatal(ctx string, err error) {
	fmt.Fprintf(os.Stderr, "Fatal: %s: %v\n", ctx, err)
	os.Exit(1)
}
