package main

import (
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Listen            string
	CPAURL            string
	ManagementKey     string
	Salt              []byte
	StateDir          string
	KeeperURL         string
	KeeperPassword    string
	RefreshOwner      string
	SnapshotEvery     time.Duration
	InterestFor       time.Duration
	CodexInterval     time.Duration
	ClaudeInterval    time.Duration
	CodexStale        time.Duration
	ClaudeStale       time.Duration
	MaxBackoff        time.Duration
	CodexBankedResets bool
	BankedInterval    time.Duration
	BankedStale       time.Duration
}

func loadConfig() (Config, error) {
	c := Config{Listen: env("LISTEN_ADDR", ":8080"), CPAURL: strings.TrimRight(env("CPA_URL", "http://cli-proxy-api:8317"), "/"), StateDir: env("STATE_DIR", "/data"), KeeperURL: strings.TrimRight(os.Getenv("KEEPER_URL"), "/"), RefreshOwner: env("REFRESH_OWNER", "keeper")}
	var err error
	c.CodexBankedResets, err = strconv.ParseBool(env("CODEX_BANKED_RESETS", "false"))
	if err != nil {
		return c, errors.New("invalid banked resets policy")
	}
	c.ManagementKey, err = secret("CPA_MANAGEMENT_KEY")
	if err != nil || c.ManagementKey == "" {
		return c, errors.New("management credential required")
	}
	salt, err := secret("ACCOUNT_ID_SALT")
	if err != nil {
		return c, err
	}
	c.Salt, err = hex.DecodeString(salt)
	if err != nil || len(c.Salt) < 32 {
		return c, errors.New("salt must contain at least 32 random bytes in hex")
	}
	if c.RefreshOwner == "quota-api" {
		c.KeeperPassword, err = secret("KEEPER_PASSWORD")
		if err != nil {
			return c, err
		}
	}
	for _, item := range []struct {
		name     string
		dest     *time.Duration
		fallback time.Duration
		minimum  time.Duration
	}{
		{"SNAPSHOT_EVERY", &c.SnapshotEvery, 15 * time.Second, time.Second},
		{"INTEREST_FOR", &c.InterestFor, 2 * time.Minute, time.Second},
		{"CODEX_REFRESH_INTERVAL", &c.CodexInterval, 15 * time.Minute, time.Minute},
		{"CLAUDE_REFRESH_INTERVAL", &c.ClaudeInterval, 15 * time.Minute, time.Minute},
		{"CODEX_STALE_AFTER", &c.CodexStale, 10 * time.Minute, time.Minute},
		{"CLAUDE_STALE_AFTER", &c.ClaudeStale, 10 * time.Minute, time.Minute},
		{"MAX_BACKOFF", &c.MaxBackoff, 24 * time.Hour, time.Hour},
		{"BANKED_RESETS_INTERVAL", &c.BankedInterval, 15 * time.Minute, 15 * time.Minute},
		{"BANKED_RESETS_STALE_AFTER", &c.BankedStale, 15 * time.Minute, time.Minute},
	} {
		*item.dest, err = time.ParseDuration(env(item.name, item.fallback.String()))
		if err != nil || *item.dest < item.minimum || *item.dest > 30*24*time.Hour {
			return c, errors.New("invalid timing policy")
		}
	}
	if c.MaxBackoff < c.CodexInterval || c.MaxBackoff < c.ClaudeInterval || (c.CodexBankedResets && c.MaxBackoff < c.BankedInterval) {
		return c, errors.New("backoff below interval")
	}
	if c.RefreshOwner != "keeper" && c.RefreshOwner != "quota-api" {
		return c, errors.New("invalid refresh owner")
	}
	if c.RefreshOwner == "quota-api" && (c.KeeperURL == "" || c.KeeperPassword == "") {
		return c, errors.New("refresh ownership guard required")
	}
	for _, raw := range []string{c.CPAURL, c.KeeperURL} {
		if raw == "" {
			continue
		}
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return c, errors.New("invalid upstream origin")
		}
	}
	return c, nil
}

func env(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}
func secret(name string) (string, error) {
	v, path := os.Getenv(name), os.Getenv(name+"_FILE")
	if v != "" && path != "" {
		return "", errors.New("ambiguous secret source")
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("secret file unavailable")
		}
		v = string(data)
	}
	return strings.TrimSpace(v), nil
}
func (c Config) stale(provider string) time.Duration {
	if provider == "claude" {
		return c.ClaudeStale
	}
	return c.CodexStale
}
func (c Config) interval(provider string) time.Duration {
	if provider == "claude" {
		return c.ClaudeInterval
	}
	return c.CodexInterval
}
func lockState(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "owner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
