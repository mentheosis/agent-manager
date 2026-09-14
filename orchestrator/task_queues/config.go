package taskqueues

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

// Config is delivered by the trusted supervisor environment, never by task data.
type Config struct {
	UseIsolatedWorkspace *bool                 `json:"use_isolated_workspace,omitempty"`
	ProfilePath          string                `json:"profile_path"`
	TablePrefix          string                `json:"-"`
	QueueID              string                `json:"queue_id"`
	DSN                  string                `json:"dsn"`
	Repositories         map[string]string     `json:"repositories"`
	Tasks                map[string]Definition `json:"tasks"`
	WorkspaceRoot        string                `json:"workspace_root"`
	InitialMaxWorkers    int                   `json:"initial_max_workers"`
	LeaseSeconds         int                   `json:"lease_seconds"`
	MaxAttemptTokens     int64                 `json:"max_attempt_tokens"`
	MaxAttemptSeconds    int                   `json:"max_attempt_seconds"`
	InternalToken        string                `json:"internal_token"`
	BaseURL              string                `json:"base_url"`
	Parent               string                `json:"parent"`
	ControllerID         string                `json:"controller_id"`
}

func LoadConfig() (Config, error) {
	var c Config
	if err := json.Unmarshal([]byte(os.Getenv("AM_QUEUE_CONFIG")), &c); err != nil {
		return c, errors.New("invalid AM_QUEUE_CONFIG")
	}
	if c.TablePrefix == "" {
		c.TablePrefix = "am_"
	}
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,40}$`).MatchString(c.TablePrefix) {
		return c, errors.New("invalid table prefix")
	}
	if c.InitialMaxWorkers == 0 {
		c.InitialMaxWorkers = 1
	}
	if c.InitialMaxWorkers < 1 {
		return c, errors.New("initial_max_workers outside configured range")
	}
	if c.LeaseSeconds == 0 {
		c.LeaseSeconds = 60
	}
	if c.MaxAttemptSeconds == 0 {
		c.MaxAttemptSeconds = 3600
	}
	if c.MaxAttemptTokens == 0 {
		c.MaxAttemptTokens = 2000000
	}
	if c.MaxAttemptTokens < 1 {
		return c, errors.New("invalid attempt budget")
	}
	if (c.QueueID != "" && !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`).MatchString(c.QueueID)) || c.DSN == "" || len(c.InternalToken) < 32 || !filepath.IsAbs(c.WorkspaceRoot) || c.LeaseSeconds < 15 || c.MaxAttemptSeconds < c.LeaseSeconds {
		return c, errors.New("invalid queue configuration")
	}
	return c, nil
}
func capability(secret, attempt string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte("queue-attempt:" + attempt))
	return hex.EncodeToString(h.Sum(nil))
}

func isolated(value *bool) bool { return value == nil || *value }
