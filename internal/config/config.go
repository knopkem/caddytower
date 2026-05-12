package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	HTTPAddr                  string
	PublicBaseURL             string
	DataDir                   string
	CaddyAdminURL             string
	RootDomain                string
	DockerHost                string
	MasterKey                 string
	BackupsEnabled            bool
	BackupsRetentionDays      int
	BackupsScheduleUTC        string
	BackupsIncludeEngineDumps bool
}

func Load() (Config, error) {
	return LoadFromLookup(os.LookupEnv)
}

func LoadFromLookup(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		HTTPAddr:      envOrDefault(lookup, "CADDYTOWER_HTTP_ADDR", ":8080"),
		PublicBaseURL: envOrDefault(lookup, "CADDYTOWER_PUBLIC_BASE_URL", "http://localhost:8080"),
		DataDir:       envOrDefault(lookup, "CADDYTOWER_DATA_DIR", "./var"),
		CaddyAdminURL: envOrDefault(lookup, "CADDYTOWER_CADDY_ADMIN_URL", "http://shared-caddy:2019"),
		RootDomain:    strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_ROOT_DOMAIN", "")),
		DockerHost:    strings.TrimSpace(envOrDefault(lookup, "DOCKER_HOST", "")),
		MasterKey:     strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_MASTER_KEY", "")),
	}
	backupsEnabled, err := envBoolOrDefault(lookup, "CADDYTOWER_BACKUPS_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	cfg.BackupsEnabled = backupsEnabled
	includeEngineDumps, err := envBoolOrDefault(lookup, "CADDYTOWER_BACKUPS_INCLUDE_ENGINE_DUMPS", true)
	if err != nil {
		return Config{}, err
	}
	cfg.BackupsIncludeEngineDumps = includeEngineDumps

	retentionDays, err := envIntOrDefault(lookup, "CADDYTOWER_BACKUPS_RETENTION_DAYS", 14)
	if err != nil {
		return Config{}, err
	}
	cfg.BackupsRetentionDays = retentionDays
	cfg.BackupsScheduleUTC = strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_BACKUPS_SCHEDULE_UTC", "02:30"))

	if strings.TrimSpace(cfg.HTTPAddr) == "" {
		return Config{}, fmt.Errorf("CADDYTOWER_HTTP_ADDR must not be empty")
	}

	if err := validateURL("CADDYTOWER_PUBLIC_BASE_URL", cfg.PublicBaseURL); err != nil {
		return Config{}, err
	}
	if err := validatePublicExposure(cfg.PublicBaseURL, cfg.MasterKey); err != nil {
		return Config{}, err
	}

	if err := validateURL("CADDYTOWER_CADDY_ADMIN_URL", cfg.CaddyAdminURL); err != nil {
		return Config{}, err
	}

	if cfg.BackupsRetentionDays < 1 {
		return Config{}, fmt.Errorf("CADDYTOWER_BACKUPS_RETENTION_DAYS must be at least 1")
	}

	if err := validateClock("CADDYTOWER_BACKUPS_SCHEDULE_UTC", cfg.BackupsScheduleUTC); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) StateDBPath() string {
	return filepath.Join(c.DataDir, "state.db")
}

func (c Config) BackupDir() string {
	return filepath.Join(c.DataDir, "backups")
}

func envOrDefault(lookup func(string) (string, bool), key, fallback string) string {
	value, ok := lookup(key)
	if !ok {
		return fallback
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}

	return value
}

func envBoolOrDefault(lookup func(string) (string, bool), key string, fallback bool) (bool, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}

	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "":
		return fallback, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be a boolean", key)
	}
}

func envIntOrDefault(lookup func(string) (string, bool), key string, fallback int) (int, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}

	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return parsed, nil
}

func validateURL(name, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is invalid: %w", name, err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s must include scheme and host", name)
	}

	return nil
}

func validatePublicExposure(publicBaseURL, masterKey string) error {
	parsed, err := url.Parse(publicBaseURL)
	if err != nil {
		return fmt.Errorf("CADDYTOWER_PUBLIC_BASE_URL is invalid: %w", err)
	}

	if isLocalHost(parsed.Hostname()) {
		return nil
	}

	// Non-local deployments are expected to be exposed through Caddy. Refusing
	// public HTTP prevents insecure cookies and accidental credential leakage.
	if parsed.Scheme != "https" {
		return fmt.Errorf("CADDYTOWER_PUBLIC_BASE_URL must use https for non-local hosts")
	}

	if strings.TrimSpace(masterKey) == "" {
		return fmt.Errorf("CADDYTOWER_MASTER_KEY is required for non-local public URLs")
	}

	return nil
}

func isLocalHost(host string) bool {
	normalized := strings.Trim(strings.ToLower(host), "[]")
	if normalized == "localhost" {
		return true
	}
	parsed := net.ParseIP(normalized)
	return parsed != nil && parsed.IsLoopback()
}

func validateClock(name, raw string) error {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return fmt.Errorf("%s must use HH:MM 24-hour format", name)
	}

	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return fmt.Errorf("%s must use HH:MM 24-hour format", name)
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return fmt.Errorf("%s must use HH:MM 24-hour format", name)
	}
	return nil
}
