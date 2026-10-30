package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/orestislef/agentic-chat/internal/hub"
)

// Registration modes.
const (
	RegistrationInvite = "invite" // first account is free, everyone after needs an invite
	RegistrationOpen   = "open"   // anyone can create an account
	RegistrationClosed = "closed" // no new accounts, not even with an invite
)

// Config is the runtime configuration, read from the environment.
type Config struct {
	Addr          string
	DataDir       string
	DownloadsDir  string
	Registration  string
	RetentionDays int
	BackupKeep    int
	TrustProxy    bool
	Demo          bool
	// MaxConnections caps long lived requests, zero means no cap.
	MaxConnections int
	Limits         hub.Limits
}

// DatabasePath is where the SQLite file lives.
func (c Config) DatabasePath() string {
	return filepath.Join(c.DataDir, "agentic-chat.db")
}

// ConfigFromEnv reads AC_* variables and applies defaults.
func ConfigFromEnv() (Config, error) {
	c := Config{
		Addr:         env("AC_ADDR", ":8080"),
		DataDir:      env("AC_DATA_DIR", "./data"),
		DownloadsDir: env("AC_DOWNLOADS_DIR", "./dist"),
		Registration: strings.ToLower(env("AC_REGISTRATION", RegistrationInvite)),
		BackupKeep:   7,
		Limits:       hub.DefaultLimits(),
	}
	switch c.Registration {
	case RegistrationInvite, RegistrationOpen, RegistrationClosed:
	default:
		return c, fmt.Errorf("AC_REGISTRATION must be invite, open or closed, got %q", c.Registration)
	}
	ints := []struct {
		name string
		dest *int
		min  int
	}{
		{"AC_RETENTION_DAYS", &c.RetentionDays, 0},
		{"AC_BACKUP_KEEP", &c.BackupKeep, 0},
		{"AC_MAX_CONNECTIONS", &c.MaxConnections, 0},
		{"AC_MAX_THREAD_MESSAGES", &c.Limits.MaxThreadMessages, 2},
		{"AC_MAX_MESSAGE_BYTES", &c.Limits.MaxMessageBytes, 1},
		{"AC_MESSAGES_PER_MINUTE", &c.Limits.MessagesPerMinute, 1},
	}
	for _, v := range ints {
		raw := os.Getenv(v.name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < v.min {
			return c, fmt.Errorf("%s must be a number of at least %d, got %q", v.name, v.min, raw)
		}
		*v.dest = n
	}
	if c.Limits.MaxMessageBytes > maxMessageBytesCap {
		return c, fmt.Errorf("AC_MAX_MESSAGE_BYTES can be at most %d", maxMessageBytesCap)
	}
	bools := []struct {
		name string
		dest *bool
	}{
		{"AC_TRUST_PROXY", &c.TrustProxy},
		{"AC_DEMO", &c.Demo},
	}
	for _, v := range bools {
		raw := os.Getenv(v.name)
		if raw == "" {
			continue
		}
		on, err := strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("%s must be true or false, got %q", v.name, raw)
		}
		*v.dest = on
	}
	return c, nil
}

// maxMessageBytesCap keeps a message inside the request size limits, even when
// JSON escaping makes it several times larger on the wire.
const maxMessageBytesCap = 128 * 1024

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
