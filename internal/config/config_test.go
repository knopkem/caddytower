package config

import "testing"

func TestLoadFromLookupDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromLookup(func(string) (string, bool) {
		return "", false
	})
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}

	if cfg.PublicBaseURL != "http://localhost:8080" {
		t.Fatalf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}

	if cfg.DataDir != "./var" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}

	if cfg.CaddyAdminURL != "http://shared-caddy:2019" {
		t.Fatalf("CaddyAdminURL = %q", cfg.CaddyAdminURL)
	}
	if cfg.BackupsEnabled {
		t.Fatal("BackupsEnabled should default to false")
	}
	if cfg.BackupsRetentionDays != 14 {
		t.Fatalf("BackupsRetentionDays = %d", cfg.BackupsRetentionDays)
	}
	if cfg.BackupsScheduleUTC != "02:30" {
		t.Fatalf("BackupsScheduleUTC = %q", cfg.BackupsScheduleUTC)
	}
	if !cfg.BackupsIncludeEngineDumps {
		t.Fatal("BackupsIncludeEngineDumps should default to true")
	}
	if !cfg.VPSWarningsEnabled || cfg.VPSRAMFreeWarnPercent != 15 || cfg.VPSDiskFreeWarnPercent != 15 {
		t.Fatalf("unexpected warning defaults: %#v", cfg)
	}
	if cfg.VPSWarningCheckMinutes != 15 || cfg.VPSWarningCooldownMinutes != 360 {
		t.Fatalf("unexpected warning timing defaults: %#v", cfg)
	}
}

func TestLoadFromLookupRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	_, err := LoadFromLookup(func(key string) (string, bool) {
		if key == "CADDYTOWER_PUBLIC_BASE_URL" {
			return "not-a-url", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected error for invalid public base url")
	}
}

func TestLoadFromLookupRejectsNonLocalHTTPPublicURL(t *testing.T) {
	t.Parallel()

	_, err := LoadFromLookup(func(key string) (string, bool) {
		switch key {
		case "CADDYTOWER_PUBLIC_BASE_URL":
			return "http://admin.example.com", true
		case "CADDYTOWER_MASTER_KEY":
			return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa=", true
		default:
			return "", false
		}
	})
	if err == nil {
		t.Fatal("expected error for non-local http public url")
	}
}

func TestLoadFromLookupRejectsNonLocalPublicURLWithoutMasterKey(t *testing.T) {
	t.Parallel()

	_, err := LoadFromLookup(func(key string) (string, bool) {
		if key == "CADDYTOWER_PUBLIC_BASE_URL" {
			return "https://admin.example.com", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected error for missing master key")
	}
}

func TestLoadFromLookupAcceptsSecureNonLocalPublicURL(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromLookup(func(key string) (string, bool) {
		switch key {
		case "CADDYTOWER_PUBLIC_BASE_URL":
			return "https://admin.example.com", true
		case "CADDYTOWER_MASTER_KEY":
			return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa=", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}
	if cfg.PublicBaseURL != "https://admin.example.com" {
		t.Fatalf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}
}

func TestLoadFromLookupParsesBackupSettings(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromLookup(func(key string) (string, bool) {
		switch key {
		case "CADDYTOWER_BACKUPS_ENABLED":
			return "true", true
		case "CADDYTOWER_BACKUPS_RETENTION_DAYS":
			return "3", true
		case "CADDYTOWER_BACKUPS_SCHEDULE_UTC":
			return "06:45", true
		case "CADDYTOWER_BACKUPS_INCLUDE_ENGINE_DUMPS":
			return "false", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}

	if !cfg.BackupsEnabled || cfg.BackupsRetentionDays != 3 || cfg.BackupsScheduleUTC != "06:45" || cfg.BackupsIncludeEngineDumps {
		t.Fatalf("unexpected backup config: %#v", cfg)
	}
}

func TestLoadFromLookupRejectsInvalidBackupSchedule(t *testing.T) {
	t.Parallel()

	_, err := LoadFromLookup(func(key string) (string, bool) {
		if key == "CADDYTOWER_BACKUPS_SCHEDULE_UTC" {
			return "25:99", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected error for invalid backup schedule")
	}
}

func TestLoadFromLookupRejectsInvalidBackupBool(t *testing.T) {
	t.Parallel()

	_, err := LoadFromLookup(func(key string) (string, bool) {
		if key == "CADDYTOWER_BACKUPS_ENABLED" {
			return "maybe", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected error for invalid backup bool")
	}
}

func TestLoadFromLookupParsesWarningEmailSettings(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromLookup(func(key string) (string, bool) {
		switch key {
		case "CADDYTOWER_VPS_RAM_FREE_WARN_PERCENT":
			return "20", true
		case "CADDYTOWER_VPS_DISK_FREE_WARN_PERCENT":
			return "25", true
		case "CADDYTOWER_VPS_WARNING_CHECK_MINUTES":
			return "5", true
		case "CADDYTOWER_VPS_WARNING_COOLDOWN_MINUTES":
			return "60", true
		case "CADDYTOWER_SMTP_HOST":
			return "smtp.example.com", true
		case "CADDYTOWER_SMTP_PORT":
			return "2525", true
		case "CADDYTOWER_SMTP_FROM":
			return "caddytower@example.com", true
		case "CADDYTOWER_SMTP_TO":
			return "ops@example.com", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}

	if cfg.VPSRAMFreeWarnPercent != 20 || cfg.VPSDiskFreeWarnPercent != 25 || cfg.VPSWarningCheckMinutes != 5 || cfg.VPSWarningCooldownMinutes != 60 {
		t.Fatalf("unexpected warning config: %#v", cfg)
	}
	if !cfg.WarningEmailConfigured() {
		t.Fatal("WarningEmailConfigured() should be true")
	}
}

func TestLoadFromLookupRejectsInvalidWarningPercent(t *testing.T) {
	t.Parallel()

	_, err := LoadFromLookup(func(key string) (string, bool) {
		if key == "CADDYTOWER_VPS_RAM_FREE_WARN_PERCENT" {
			return "0", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected error for invalid warning percent")
	}
}
