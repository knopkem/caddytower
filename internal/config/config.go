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
	GitHubAppID               int64
	GitHubAppSlug             string
	GitHubAppPrivateKeyPath   string
	GitHubWebhookSecret       string
	GitHubAPIBaseURL          string
	GitHubWebBaseURL          string
	BackupsEnabled            bool
	BackupsRetentionDays      int
	BackupsScheduleUTC        string
	BackupsIncludeEngineDumps bool
	VPSWarningsEnabled        bool
	VPSRAMFreeWarnPercent     int
	VPSDiskFreeWarnPercent    int
	VPSWarningCheckMinutes    int
	VPSWarningCooldownMinutes int
	SMTPHost                  string
	SMTPPort                  int
	SMTPUsername              string
	SMTPPassword              string
	SMTPFrom                  string
	SMTPTo                    string
}

func Load() (Config, error) {
	return LoadFromLookup(os.LookupEnv)
}

func LoadFromLookup(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		HTTPAddr:                envOrDefault(lookup, "CADDYTOWER_HTTP_ADDR", ":8080"),
		PublicBaseURL:           envOrDefault(lookup, "CADDYTOWER_PUBLIC_BASE_URL", "http://localhost:8080"),
		DataDir:                 envOrDefault(lookup, "CADDYTOWER_DATA_DIR", "./var"),
		CaddyAdminURL:           envOrDefault(lookup, "CADDYTOWER_CADDY_ADMIN_URL", "http://shared-caddy:2019"),
		RootDomain:              strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_ROOT_DOMAIN", "")),
		DockerHost:              strings.TrimSpace(envOrDefault(lookup, "DOCKER_HOST", "")),
		MasterKey:               strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_MASTER_KEY", "")),
		GitHubAppSlug:           strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_GITHUB_APP_SLUG", "")),
		GitHubAppPrivateKeyPath: strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH", "")),
		GitHubWebhookSecret:     strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_GITHUB_WEBHOOK_SECRET", "")),
		GitHubAPIBaseURL:        envOrDefault(lookup, "CADDYTOWER_GITHUB_API_BASE_URL", "https://api.github.com"),
		GitHubWebBaseURL:        envOrDefault(lookup, "CADDYTOWER_GITHUB_WEB_BASE_URL", "https://github.com"),
	}
	appID, err := envInt64OrDefault(lookup, "CADDYTOWER_GITHUB_APP_ID", 0)
	if err != nil {
		return Config{}, err
	}
	cfg.GitHubAppID = appID
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

	warningsEnabled, err := envBoolOrDefault(lookup, "CADDYTOWER_VPS_WARNINGS_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	cfg.VPSWarningsEnabled = warningsEnabled
	ramWarnPercent, err := envIntOrDefault(lookup, "CADDYTOWER_VPS_RAM_FREE_WARN_PERCENT", 15)
	if err != nil {
		return Config{}, err
	}
	cfg.VPSRAMFreeWarnPercent = ramWarnPercent
	diskWarnPercent, err := envIntOrDefault(lookup, "CADDYTOWER_VPS_DISK_FREE_WARN_PERCENT", 15)
	if err != nil {
		return Config{}, err
	}
	cfg.VPSDiskFreeWarnPercent = diskWarnPercent
	checkMinutes, err := envIntOrDefault(lookup, "CADDYTOWER_VPS_WARNING_CHECK_MINUTES", 15)
	if err != nil {
		return Config{}, err
	}
	cfg.VPSWarningCheckMinutes = checkMinutes
	cooldownMinutes, err := envIntOrDefault(lookup, "CADDYTOWER_VPS_WARNING_COOLDOWN_MINUTES", 360)
	if err != nil {
		return Config{}, err
	}
	cfg.VPSWarningCooldownMinutes = cooldownMinutes
	smtpPort, err := envIntOrDefault(lookup, "CADDYTOWER_SMTP_PORT", 587)
	if err != nil {
		return Config{}, err
	}
	cfg.SMTPPort = smtpPort
	cfg.SMTPHost = strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_SMTP_HOST", ""))
	cfg.SMTPUsername = strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_SMTP_USERNAME", ""))
	cfg.SMTPPassword = strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_SMTP_PASSWORD", ""))
	cfg.SMTPFrom = strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_SMTP_FROM", ""))
	cfg.SMTPTo = strings.TrimSpace(envOrDefault(lookup, "CADDYTOWER_SMTP_TO", ""))

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
	if err := validateURL("CADDYTOWER_GITHUB_API_BASE_URL", cfg.GitHubAPIBaseURL); err != nil {
		return Config{}, err
	}
	if err := validateURL("CADDYTOWER_GITHUB_WEB_BASE_URL", cfg.GitHubWebBaseURL); err != nil {
		return Config{}, err
	}
	if err := validateGitHubAppConfig(cfg); err != nil {
		return Config{}, err
	}

	if cfg.BackupsRetentionDays < 1 {
		return Config{}, fmt.Errorf("CADDYTOWER_BACKUPS_RETENTION_DAYS must be at least 1")
	}

	if err := validateClock("CADDYTOWER_BACKUPS_SCHEDULE_UTC", cfg.BackupsScheduleUTC); err != nil {
		return Config{}, err
	}
	if err := validatePercent("CADDYTOWER_VPS_RAM_FREE_WARN_PERCENT", cfg.VPSRAMFreeWarnPercent); err != nil {
		return Config{}, err
	}
	if err := validatePercent("CADDYTOWER_VPS_DISK_FREE_WARN_PERCENT", cfg.VPSDiskFreeWarnPercent); err != nil {
		return Config{}, err
	}
	if cfg.VPSWarningCheckMinutes < 1 {
		return Config{}, fmt.Errorf("CADDYTOWER_VPS_WARNING_CHECK_MINUTES must be at least 1")
	}
	if cfg.VPSWarningCooldownMinutes < 1 {
		return Config{}, fmt.Errorf("CADDYTOWER_VPS_WARNING_COOLDOWN_MINUTES must be at least 1")
	}
	if cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
		return Config{}, fmt.Errorf("CADDYTOWER_SMTP_PORT must be between 1 and 65535")
	}

	return cfg, nil
}

func (c Config) StateDBPath() string {
	return filepath.Join(c.DataDir, "state.db")
}

func (c Config) BackupDir() string {
	return filepath.Join(c.DataDir, "backups")
}

func (c Config) WarningEmailConfigured() bool {
	return c.SMTPHost != "" && c.SMTPFrom != "" && c.SMTPTo != ""
}

func (c Config) GitHubConfigured() bool {
	return c.GitHubAppID > 0 &&
		c.GitHubAppSlug != "" &&
		c.GitHubAppPrivateKeyPath != "" &&
		c.GitHubWebhookSecret != ""
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

func envInt64OrDefault(lookup func(string) (string, bool), key string, fallback int64) (int64, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
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

func validateGitHubAppConfig(cfg Config) error {
	values := []string{
		strconv.FormatInt(cfg.GitHubAppID, 10),
		cfg.GitHubAppSlug,
		cfg.GitHubAppPrivateKeyPath,
		cfg.GitHubWebhookSecret,
	}
	anySet := false
	for _, value := range values {
		if strings.TrimSpace(value) != "" && value != "0" {
			anySet = true
			break
		}
	}
	if !anySet {
		return nil
	}
	if cfg.GitHubAppID <= 0 {
		return fmt.Errorf("CADDYTOWER_GITHUB_APP_ID must be set when GitHub App integration is enabled")
	}
	if cfg.GitHubAppSlug == "" {
		return fmt.Errorf("CADDYTOWER_GITHUB_APP_SLUG must be set when GitHub App integration is enabled")
	}
	if cfg.GitHubAppPrivateKeyPath == "" {
		return fmt.Errorf("CADDYTOWER_GITHUB_APP_PRIVATE_KEY_PATH must be set when GitHub App integration is enabled")
	}
	if cfg.GitHubWebhookSecret == "" {
		return fmt.Errorf("CADDYTOWER_GITHUB_WEBHOOK_SECRET must be set when GitHub App integration is enabled")
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

func validatePercent(name string, value int) error {
	if value < 1 || value > 99 {
		return fmt.Errorf("%s must be between 1 and 99", name)
	}
	return nil
}
