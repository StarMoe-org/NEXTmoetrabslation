// Package backup provides daily and manual backup/restore of translation data to
// two independently-configurable targets: an S3-compatible bucket (tarball) and
// a GitHub repository (git commit/push). Each target can use a different repo
// or bucket and branch.
package backup

import (
	"fmt"
	"log"
	"sync"
	"time"

	"moesekai/server/internal/config"
	"moesekai/server/internal/files"
	"moesekai/server/internal/importer"
	"moesekai/server/internal/store"
)

// Status reports the last backup/restore outcome per target.
type Status struct {
	Running       bool   `json:"running"`
	S3Enabled     bool   `json:"s3Enabled"`
	GitEnabled    bool   `json:"gitEnabled"`
	LastBackup    string `json:"lastBackup,omitempty"`
	LastS3Backup  string `json:"lastS3Backup,omitempty"`
	LastGitBackup string `json:"lastGitBackup,omitempty"`
	LastRestore   string `json:"lastRestore,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	DailyHourUTC  int    `json:"dailyHourUtc"`
}

// Manager coordinates backup targets and the daily schedule.
type Manager struct {
	cfg      *config.Config
	gen      *files.Generator
	store    *store.Store
	eventStr *store.EventStore
	workDir  string // scratch space for tarballs / git clones

	mu     sync.Mutex
	status Status
	stopCh chan struct{}
}

func NewManager(cfg *config.Config, gen *files.Generator, s *store.Store, es *store.EventStore, workDir string) *Manager {
	return &Manager{
		cfg:      cfg,
		gen:      gen,
		store:    s,
		eventStr: es,
		workDir:  workDir,
		stopCh:   make(chan struct{}),
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	s.S3Enabled = m.cfg.GetBool(config.KeyBackupS3Enabled, false)
	s.GitEnabled = m.cfg.GetBool(config.KeyBackupGitEnabled, false)
	s.DailyHourUTC = m.cfg.GetInt(config.KeyBackupDailyHour, 19) // 19 UTC = 03:00 UTC+8
	return s
}

func (m *Manager) setError(err error) {
	m.mu.Lock()
	if err != nil {
		m.status.LastError = err.Error()
	} else {
		m.status.LastError = ""
	}
	m.mu.Unlock()
}

// StartScheduler runs a daily backup at the configured UTC hour.
func (m *Manager) StartScheduler() {
	go m.scheduleLoop()
}

func (m *Manager) Stop() { close(m.stopCh) }

func (m *Manager) scheduleLoop() {
	// Check every 30 min whether we've crossed into the target hour today.
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	lastRunDay := ""
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			now := time.Now().UTC()
			hour := m.cfg.GetInt(config.KeyBackupDailyHour, 19)
			day := now.Format("2006-01-02")
			if now.Hour() == hour && day != lastRunDay {
				lastRunDay = day
				log.Printf("[backup] daily scheduled backup starting (hour=%d UTC)", hour)
				if _, err := m.BackupAll(); err != nil {
					log.Printf("[backup] daily backup failed: %v", err)
				}
			}
		}
	}
}

// BackupAll runs every enabled backup target. Returns a per-target summary.
func (m *Manager) BackupAll() (map[string]string, error) {
	m.mu.Lock()
	if m.status.Running {
		m.mu.Unlock()
		return nil, fmt.Errorf("a backup is already running")
	}
	m.status.Running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.status.Running = false
		m.mu.Unlock()
	}()

	results := map[string]string{}
	var firstErr error

	log.Printf("[backup] backup starting (s3=%v git=%v)",
		m.cfg.GetBool(config.KeyBackupS3Enabled, false), m.cfg.GetBool(config.KeyBackupGitEnabled, false))

	if m.cfg.GetBool(config.KeyBackupS3Enabled, false) {
		if err := m.backupS3(); err != nil {
			log.Printf("[backup] s3 failed: %v", err)
			results["s3"] = "error: " + err.Error()
			if firstErr == nil {
				firstErr = err
			}
		} else {
			log.Printf("[backup] s3 ok")
			results["s3"] = "ok"
			m.mu.Lock()
			m.status.LastS3Backup = nowRFC3339()
			m.mu.Unlock()
		}
	} else {
		results["s3"] = "disabled"
	}

	if m.cfg.GetBool(config.KeyBackupGitEnabled, false) {
		if err := m.backupGit(); err != nil {
			log.Printf("[backup] git failed: %v", err)
			results["git"] = "error: " + err.Error()
			if firstErr == nil {
				firstErr = err
			}
		} else {
			log.Printf("[backup] git ok")
			results["git"] = "ok"
			m.mu.Lock()
			m.status.LastGitBackup = nowRFC3339()
			m.mu.Unlock()
		}
	} else {
		results["git"] = "disabled"
	}

	m.setError(firstErr)
	if firstErr == nil {
		m.mu.Lock()
		m.status.LastBackup = nowRFC3339()
		m.mu.Unlock()
	}
	return results, firstErr
}

// RestoreFrom restores translations from the named target ("s3" or "git") and
// imports them into the stores, replacing current data.
func (m *Manager) RestoreFrom(target string) (importer.Result, error) {
	m.mu.Lock()
	if m.status.Running {
		m.mu.Unlock()
		return importer.Result{}, fmt.Errorf("a backup/restore is already running")
	}
	m.status.Running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.status.Running = false
		m.mu.Unlock()
	}()

	var res importer.Result
	var err error
	log.Printf("[backup] restore starting (target=%s)", target)
	switch target {
	case "s3":
		res, err = m.restoreS3()
	case "git":
		res, err = m.restoreGit()
	default:
		return res, fmt.Errorf("unknown restore target: %s", target)
	}
	if err != nil {
		log.Printf("[backup] restore from %s failed: %v", target, err)
		m.setError(err)
		return res, err
	}
	log.Printf("[backup] restore from %s ok: %d categories, %d entries, %d event stories",
		target, res.Categories, res.Entries, res.EventStories)
	m.mu.Lock()
	m.status.LastRestore = nowRFC3339()
	m.status.LastError = ""
	m.mu.Unlock()
	return res, nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
