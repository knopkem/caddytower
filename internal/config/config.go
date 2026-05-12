package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	HTTPAddr      string
	PublicBaseURL string
	DataDir       string
	CaddyAdminURL string
	RootDomain    string
	DockerHost    string
	MasterKey     string
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

	if strings.TrimSpace(cfg.HTTPAddr) == "" {
		return Config{}, fmt.Errorf("CADDYTOWER_HTTP_ADDR must not be empty")
	}

	if err := validateURL("CADDYTOWER_PUBLIC_BASE_URL", cfg.PublicBaseURL); err != nil {
		return Config{}, err
	}

	if err := validateURL("CADDYTOWER_CADDY_ADMIN_URL", cfg.CaddyAdminURL); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) StateDBPath() string {
	return filepath.Join(c.DataDir, "state.db")
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
