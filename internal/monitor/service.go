package monitor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"caddytower/internal/config"
)

type Service struct {
	cfg      config.Config
	logger   *slog.Logger
	mailer   emailSender
	now      func() time.Time
	collect  func(config.Config, time.Time) (Status, error)
	lastSent map[string]time.Time
}

type Status struct {
	CollectedAt     time.Time
	Memory          Resource
	Disk            Resource
	DiskPath        string
	Warnings        []string
	EmailConfigured bool
	EmailTo         string
	RAMThreshold    int
	DiskThreshold   int
}

type Resource struct {
	UsedBytes   uint64
	TotalBytes  uint64
	UsedPercent int
	FreePercent int
}

type emailSender interface {
	Send(ctx context.Context, subject, body string) error
}

func New(cfg config.Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg:      cfg,
		logger:   logger,
		mailer:   newSMTPMailer(cfg),
		now:      time.Now,
		collect:  collectStatus,
		lastSent: map[string]time.Time{},
	}
}

func (s *Service) Start(ctx context.Context) {
	if !s.cfg.VPSWarningsEnabled {
		return
	}

	interval := time.Duration(s.cfg.VPSWarningCheckMinutes) * time.Minute
	go func() {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.checkAndNotify(ctx)
				timer.Reset(interval)
			}
		}
	}()
}

func (s *Service) Snapshot() (Status, error) {
	collect := s.collect
	if collect == nil {
		collect = collectStatus
	}
	status, err := collect(s.cfg, s.now())
	if err != nil {
		return Status{}, err
	}
	return status, nil
}

func (s *Service) checkAndNotify(ctx context.Context) {
	status, err := s.Snapshot()
	if err != nil {
		s.logger.Warn("collect vps status", "error", err)
		return
	}
	if len(status.Warnings) == 0 {
		return
	}
	if !status.EmailConfigured {
		s.logger.Warn("vps resource warning", "warnings", strings.Join(status.Warnings, "; "), "email_configured", false)
		return
	}

	key := strings.Join(status.Warnings, "|")
	cooldown := time.Duration(s.cfg.VPSWarningCooldownMinutes) * time.Minute
	if last := s.lastSent[key]; !last.IsZero() && s.now().Sub(last) < cooldown {
		return
	}

	body := fmt.Sprintf("CaddyTower detected low VPS resources at %s.\n\n%s\n\nMemory: %s used of %s (%d%% used)\nDisk: %s used of %s (%d%% used)\nDisk path: %s\n",
		status.CollectedAt.Format(time.RFC3339),
		strings.Join(status.Warnings, "\n"),
		humanBytes(status.Memory.UsedBytes),
		humanBytes(status.Memory.TotalBytes),
		status.Memory.UsedPercent,
		humanBytes(status.Disk.UsedBytes),
		humanBytes(status.Disk.TotalBytes),
		status.Disk.UsedPercent,
		status.DiskPath,
	)
	if err := s.mailer.Send(ctx, "CaddyTower VPS resource warning", body); err != nil {
		s.logger.Warn("send vps warning email", "error", err)
		return
	}
	s.lastSent[key] = s.now()
}

func collectStatus(cfg config.Config, now time.Time) (Status, error) {
	memory, err := collectMemory()
	if err != nil {
		return Status{}, err
	}
	disk, err := collectDisk(cfg.DataDir)
	if err != nil {
		return Status{}, err
	}

	status := Status{
		CollectedAt:     now.UTC(),
		Memory:          memory,
		Disk:            disk,
		DiskPath:        cfg.DataDir,
		EmailConfigured: cfg.WarningEmailConfigured(),
		EmailTo:         cfg.SMTPTo,
		RAMThreshold:    cfg.VPSRAMFreeWarnPercent,
		DiskThreshold:   cfg.VPSDiskFreeWarnPercent,
	}
	if memory.FreePercent < cfg.VPSRAMFreeWarnPercent {
		status.Warnings = append(status.Warnings, fmt.Sprintf("RAM available is below %d%%", cfg.VPSRAMFreeWarnPercent))
	}
	if disk.FreePercent < cfg.VPSDiskFreeWarnPercent {
		status.Warnings = append(status.Warnings, fmt.Sprintf("disk free space is below %d%%", cfg.VPSDiskFreeWarnPercent))
	}
	return status, nil
}

func collectMemory() (Resource, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Resource{}, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return parseMeminfo(data)
}

func parseMeminfo(data []byte) (Resource, error) {
	values := map[string]uint64{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		values[key] = value * 1024
	}
	if err := scanner.Err(); err != nil {
		return Resource{}, fmt.Errorf("scan meminfo: %w", err)
	}

	total := values["MemTotal"]
	available := values["MemAvailable"]
	if available == 0 {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if total == 0 || available > total {
		return Resource{}, fmt.Errorf("meminfo missing usable MemTotal/MemAvailable")
	}
	used := total - available
	return resourceFromUsedTotal(used, total), nil
}

func collectDisk(path string) (Resource, error) {
	if strings.TrimSpace(path) == "" {
		path = "."
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return Resource{}, fmt.Errorf("ensure disk status path: %w", err)
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Resource{}, fmt.Errorf("stat filesystem %s: %w", path, err)
	}
	blockSize := uint64(stat.Bsize)
	total := stat.Blocks * blockSize
	free := stat.Bavail * blockSize
	if total == 0 || free > total {
		return Resource{}, fmt.Errorf("filesystem %s returned invalid capacity", path)
	}
	return resourceFromUsedTotal(total-free, total), nil
}

func resourceFromUsedTotal(used, total uint64) Resource {
	usedPercent := 0
	freePercent := 0
	if total > 0 {
		usedPercent = int((used*100 + total/2) / total)
		freePercent = 100 - usedPercent
	}
	return Resource{
		UsedBytes:   used,
		TotalBytes:  total,
		UsedPercent: usedPercent,
		FreePercent: freePercent,
	}
}

type smtpMailer struct {
	cfg config.Config
}

func newSMTPMailer(cfg config.Config) emailSender {
	return smtpMailer{cfg: cfg}
}

func (m smtpMailer) Send(ctx context.Context, subject, body string) error {
	if !m.cfg.WarningEmailConfigured() {
		return fmt.Errorf("SMTP host/from/to are not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", m.cfg.SMTPHost, m.cfg.SMTPPort)
	headers := map[string]string{
		"From":         m.cfg.SMTPFrom,
		"To":           m.cfg.SMTPTo,
		"Subject":      subject,
		"MIME-Version": "1.0",
		"Content-Type": "text/plain; charset=utf-8",
	}
	var message strings.Builder
	for key, value := range headers {
		message.WriteString(key)
		message.WriteString(": ")
		message.WriteString(value)
		message.WriteString("\r\n")
	}
	message.WriteString("\r\n")
	message.WriteString(body)

	var auth smtp.Auth
	if m.cfg.SMTPUsername != "" || m.cfg.SMTPPassword != "" {
		auth = smtp.PlainAuth("", m.cfg.SMTPUsername, m.cfg.SMTPPassword, m.cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, m.cfg.SMTPFrom, []string{m.cfg.SMTPTo}, []byte(message.String()))
}

func humanBytes(size uint64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case size >= GB:
		return fmt.Sprintf("%.1f GB", float64(size)/GB)
	case size >= MB:
		return fmt.Sprintf("%.1f MB", float64(size)/MB)
	case size >= KB:
		return fmt.Sprintf("%.1f KB", float64(size)/KB)
	default:
		return fmt.Sprintf("%d B", size)
	}
}
