package monitor

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"caddytower/internal/config"
)

func TestParseMeminfoUsesAvailableMemory(t *testing.T) {
	t.Parallel()

	resource, err := parseMeminfo([]byte(`
MemTotal:        2048000 kB
MemFree:          100000 kB
MemAvailable:     512000 kB
Buffers:           10000 kB
Cached:            20000 kB
`))
	if err != nil {
		t.Fatalf("parseMeminfo() error = %v", err)
	}
	if resource.TotalBytes != 2048000*1024 {
		t.Fatalf("TotalBytes = %d", resource.TotalBytes)
	}
	if resource.FreePercent != 25 || resource.UsedPercent != 75 {
		t.Fatalf("percentages = used %d free %d", resource.UsedPercent, resource.FreePercent)
	}
}

func TestCheckAndNotifySendsWarningEmailOncePerCooldown(t *testing.T) {
	t.Parallel()

	mailer := &fakeMailer{}
	svc := &Service{
		cfg: config.Config{
			DataDir:                   t.TempDir(),
			VPSWarningsEnabled:        true,
			VPSRAMFreeWarnPercent:     99,
			VPSDiskFreeWarnPercent:    99,
			VPSWarningCooldownMinutes: 60,
			VPSWarningCheckMinutes:    1,
			SMTPHost:                  "smtp.example.com",
			SMTPPort:                  587,
			SMTPFrom:                  "caddytower@example.com",
			SMTPTo:                    "ops@example.com",
			BackupsRetentionDays:      14,
			BackupsScheduleUTC:        "02:30",
			BackupsIncludeEngineDumps: true,
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		mailer: mailer,
		now:    func() time.Time { return time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC) },
		collect: func(cfg config.Config, now time.Time) (Status, error) {
			return Status{
				CollectedAt:     now,
				Memory:          resourceFromUsedTotal(95, 100),
				Disk:            resourceFromUsedTotal(96, 100),
				DiskPath:        cfg.DataDir,
				Warnings:        []string{"RAM available is below 99%", "disk free space is below 99%"},
				EmailConfigured: cfg.WarningEmailConfigured(),
				EmailTo:         cfg.SMTPTo,
				RAMThreshold:    cfg.VPSRAMFreeWarnPercent,
				DiskThreshold:   cfg.VPSDiskFreeWarnPercent,
			}, nil
		},
		lastSent: map[string]time.Time{},
	}

	svc.checkAndNotify(context.Background())
	svc.checkAndNotify(context.Background())

	if mailer.count != 1 {
		t.Fatalf("mailer count = %d, want 1", mailer.count)
	}
	if !strings.Contains(mailer.body, "CaddyTower detected low VPS resources") {
		t.Fatalf("email body = %q", mailer.body)
	}
}

type fakeMailer struct {
	count int
	body  string
}

func (f *fakeMailer) Send(_ context.Context, _, body string) error {
	f.count++
	f.body = body
	return nil
}
