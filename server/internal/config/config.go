// Package config manages runtime settings stored in SQLite. Non-secret values
// are stored in plaintext; secrets (API keys, backup credentials) are encrypted
// with AES-GCM using a key derived from the MOESEKAI_MASTER_KEY env var.
//
// Settings are seeded from environment variables on first run only, so the
// admin UI becomes the source of truth thereafter.
package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"moesekai/server/internal/db"
)

// Setting keys. Secret keys must be listed in secretKeys below.
const (
	KeyLLMType       = "llm.type"       // "gemini" | "openai"
	KeyGeminiAPIKey  = "llm.gemini.key" // secret
	KeyGeminiModel   = "llm.gemini.model"
	KeyOpenAIAPIKey  = "llm.openai.key" // secret
	KeyOpenAIBaseURL = "llm.openai.base_url"
	KeyOpenAIModel   = "llm.openai.model"
	KeyBatchSize     = "translate.batch_size"
	KeyRateDelayMS   = "translate.rate_delay_ms"

	KeyUpstreamRepo       = "upstream.repo"   // e.g. Team-Haruki/haruki-sekai-master
	KeyUpstreamBranch     = "upstream.branch" // e.g. main
	KeyUpstreamVersionURL = "upstream.version_url"
	KeySchedulerOn        = "scheduler.enabled"

	KeyBackupS3Enabled   = "backup.s3.enabled"
	KeyBackupS3Endpoint  = "backup.s3.endpoint"
	KeyBackupS3Region    = "backup.s3.region"
	KeyBackupS3Bucket    = "backup.s3.bucket"
	KeyBackupS3Prefix    = "backup.s3.prefix"
	KeyBackupS3AccessKey = "backup.s3.access_key" // secret
	KeyBackupS3SecretKey = "backup.s3.secret_key" // secret

	KeyBackupGitEnabled = "backup.git.enabled"
	KeyBackupGitRepoURL = "backup.git.repo_url" // secret (may embed token)
	KeyBackupGitBranch  = "backup.git.branch"

	KeyBackupDailyHour = "backup.daily_hour" // UTC hour 0-23
)

// secretKeys are stored encrypted at rest.
var secretKeys = map[string]bool{
	KeyGeminiAPIKey:      true,
	KeyOpenAIAPIKey:      true,
	KeyBackupS3AccessKey: true,
	KeyBackupS3SecretKey: true,
	KeyBackupGitRepoURL:  true,
}

// IsSecret reports whether a setting key holds a secret value.
func IsSecret(key string) bool { return secretKeys[key] }

// Config provides typed, cached access to settings backed by SQLite.
type Config struct {
	db     *db.DB
	aesKey []byte

	mu    sync.RWMutex
	cache map[string]string // decrypted values
}

// New opens the config over the given DB. masterKey may be empty, in which case
// secret settings cannot be stored (an error is returned on write attempts).
func New(database *db.DB, masterKey string) (*Config, error) {
	c := &Config{db: database, cache: map[string]string{}}
	if masterKey != "" {
		sum := sha256.Sum256([]byte(masterKey))
		c.aesKey = sum[:]
	}
	if err := c.reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// HasMasterKey reports whether secret encryption is available.
func (c *Config) HasMasterKey() bool { return len(c.aesKey) == 32 }

func (c *Config) reload() error {
	rows, err := c.db.Query(`SELECT key, value, encrypted FROM settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cache := map[string]string{}
	for rows.Next() {
		var key, value string
		var enc int
		if err := rows.Scan(&key, &value, &enc); err != nil {
			return err
		}
		if enc == 1 {
			if !c.HasMasterKey() {
				// Cannot decrypt without the key; skip (treated as unset).
				continue
			}
			dec, err := c.decrypt(value)
			if err != nil {
				return fmt.Errorf("decrypt %s: %w", key, err)
			}
			value = dec
		}
		cache[key] = value
	}
	c.mu.Lock()
	c.cache = cache
	c.mu.Unlock()
	return rows.Err()
}

// Get returns a setting value, or the empty string if unset.
func (c *Config) Get(key string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cache[key]
}

// GetOr returns the setting value or fallback if empty.
func (c *Config) GetOr(key, fallback string) string {
	if v := c.Get(key); v != "" {
		return v
	}
	return fallback
}

// GetBool parses a boolean setting (true/1/yes). fallback used if unset.
func (c *Config) GetBool(key string, fallback bool) bool {
	v := c.Get(key)
	if v == "" {
		return fallback
	}
	switch v {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return fallback
}

// GetInt parses an integer setting. fallback used if unset or invalid.
func (c *Config) GetInt(key string, fallback int) int {
	v := c.Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// Set writes a setting, encrypting it if the key is a secret.
func (c *Config) Set(key, value string) error {
	enc := 0
	stored := value
	if IsSecret(key) {
		if !c.HasMasterKey() {
			return errors.New("cannot store secret: MOESEKAI_MASTER_KEY not configured")
		}
		e, err := c.encrypt(value)
		if err != nil {
			return err
		}
		stored = e
		enc = 1
	}
	_, err := c.db.Exec(
		`INSERT INTO settings (key, value, encrypted) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, encrypted = excluded.encrypted`,
		key, stored, enc)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cache[key] = value
	c.mu.Unlock()
	return nil
}

// SetIfAbsent writes a setting only if it is not already present. Used for
// env-based seeding so the admin UI remains authoritative after first run.
func (c *Config) SetIfAbsent(key, value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	var exists int
	err := c.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = ?`, key).Scan(&exists)
	if err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil
	}
	if err := c.Set(key, value); err != nil {
		return false, err
	}
	return true, nil
}

// All returns a snapshot of all settings, with secrets masked unless reveal is
// true. Used by the admin settings API.
func (c *Config) All(reveal bool) map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]string, len(c.cache))
	for k, v := range c.cache {
		if IsSecret(k) && !reveal && v != "" {
			out[k] = "********"
			continue
		}
		out[k] = v
	}
	return out
}

// ---- AES-GCM helpers ----

func (c *Config) encrypt(plain string) (string, error) {
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (c *Config) decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(c.aesKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
