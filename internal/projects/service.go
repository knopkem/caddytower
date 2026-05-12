package projects

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"caddytower/internal/caddyadmin"
	"caddytower/internal/cloudflare"
	"caddytower/internal/config"
	"caddytower/internal/dbengines"
	"caddytower/internal/dockerx"
	"caddytower/internal/secrets"
	"caddytower/internal/store"

	"github.com/google/uuid"
)

const (
	settingRootDomain         = "root_domain"
	settingOriginHostname     = "origin_hostname"
	settingCloudflareZoneID   = "cloudflare_zone_id"
	settingCloudflareAPIToken = "cloudflare_api_token"
	settingCloudflareProxied  = "cloudflare_proxied"
	managedNetworkName        = "edge"
	controllerSubdomain       = "caddytower"
	controllerContainerName   = "caddytower"
	projectTypeWeb            = "web"
	projectTypeTCP            = "tcp"
	projectTypeUDP            = "udp"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

type dockerService interface {
	PullImage(context.Context, string) error
	RecreateContainer(context.Context, dockerx.ContainerSpec) (dockerx.ContainerInspect, error)
	InspectContainer(context.Context, string) (dockerx.ContainerInspect, error)
	ContainerStats(context.Context, string) (dockerx.ContainerStatsSnapshot, error)
	ListContainers(context.Context) ([]dockerx.ContainerSummary, error)
	ListContainersByLabel(context.Context, string, string) ([]dockerx.ContainerSummary, error)
	RestartContainer(context.Context, string) error
	RemoveContainer(context.Context, string) error
	StreamLogs(context.Context, string, int) (io.ReadCloser, error)
	Exec(context.Context, string, []string, []string, io.Writer, io.Writer) error
}

type caddyService interface {
	ReconcileManagedRoutes(context.Context, []caddyadmin.HTTPRoute, []string) (bool, error)
}

type dbService interface {
	AttachDatabase(context.Context, string, string, string, string) (dbengines.Attachment, error)
	ListAttachments(context.Context, string) ([]dbengines.Attachment, error)
	RotateAttachmentPassword(context.Context, int64) (dbengines.Attachment, error)
	DeleteAttachment(context.Context, int64) error
}

type cloudflareClient interface {
	ValidateToken(context.Context) error
	UpsertRecord(context.Context, string, string, string, bool) (cloudflare.DNSRecord, bool, error)
	DeleteRecord(context.Context, string, string) error
}

type Service struct {
	cfg           config.Config
	store         *store.Store
	secrets       *secrets.Service
	docker        dockerService
	caddy         caddyService
	db            dbService
	logger        *slog.Logger
	newCloudflare func(string) (cloudflareClient, error)
	lookupCNAME   func(context.Context, string) (string, error)
	lookupHost    func(context.Context, string) ([]string, error)
	checkHTTP     func(context.Context, string, time.Duration) error
	deployMu      sync.RWMutex
	deploySubs    map[string]map[chan DeployEvent]struct{}
}

type Settings struct {
	RootDomain             string
	OriginHostname         string
	CloudflareZoneID       string
	CloudflareProxied      bool
	CloudflareTokenPresent bool
}

type SettingsInput struct {
	RootDomain        string
	OriginHostname    string
	CloudflareZoneID  string
	CloudflareToken   string
	CloudflareProxied bool
}

type Project struct {
	ID                        string
	Name                      string
	Slug                      string
	Type                      string
	WebhookSecret             string
	ImageRef                  string
	GitHubRepoFullName        string
	GitHubInstallationID      int64
	GitHubDefaultBranch       string
	Subdomain                 string
	FullDomain                string
	ContainerName             string
	InternalPort              int
	Ports                     []ProjectPort
	WatchtowerEnabled         bool
	Env                       map[string]string
	EnvText                   string
	HealthCheckPath           string
	HealthCheckTimeoutSeconds int
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
	Status                    string
	PrimaryDomain             string
	CustomDomains             []ProjectDomain
	Deploys                   []ProjectDeploy
	DBAttachments             []DBAttachment
}

type ProjectDomain struct {
	ID            int64
	Hostname      string
	IsPrimary     bool
	DNSVerifiedAt time.Time
}

type ProjectDeploy struct {
	ID          int64
	ImageDigest string
	ImageRef    string
	Status      string
	Trigger     string
	Actor       string
	StartedAt   time.Time
	FinishedAt  time.Time
	Error       string
}

type DeployEvent struct {
	ProjectID string
	Stage     string
	Message   string
	Status    string
	Timestamp time.Time
}

type projectDeployRequest struct {
	Trigger             string
	Actor               string
	DisableAutoRollback bool
}

type Dashboard struct {
	Settings Settings
	Projects []Project
}

type AuditLogEntry struct {
	ID        string
	Timestamp time.Time
	UserEmail string
	Action    string
	Target    string
	Payload   string
}

type ProjectRuntimeSnapshot struct {
	Available        bool
	Status           string
	ReadAt           time.Time
	CPUPercent       float64
	MemoryUsageBytes uint64
	MemoryLimitBytes uint64
	MemoryPercent    int
	NetworkRxBytes   uint64
	NetworkTxBytes   uint64
	BlockReadBytes   uint64
	BlockWriteBytes  uint64
	PIDs             uint64
	Warnings         []string
}

type WebProjectInput struct {
	Type                      string
	ID                        string
	Name                      string
	Slug                      string
	ImageRef                  string
	GitHubRepoFullName        string
	GitHubInstallationID      int64
	GitHubDefaultBranch       string
	Subdomain                 string
	InternalPort              int
	PortMappingsText          string
	WatchtowerEnabled         bool
	EnvText                   string
	HealthCheckPath           string
	HealthCheckTimeoutSeconds int
	SkipDeploy                bool
}

type ProjectPort struct {
	Proto         string
	HostPort      int
	ContainerPort int
}

type DBAttachment struct {
	ID             int64
	Engine         string
	DBName         string
	DBUser         string
	EnvVarName     string
	Host           string
	Port           int
	Password       string
	ConnectionHint string
}

type DatabaseAttachmentInput struct {
	ProjectID  string
	Engine     string
	EnvVarName string
}

func New(cfg config.Config, stateStore *store.Store, secretService *secrets.Service, dockerSvc dockerService, caddySvc caddyService, logger *slog.Logger) *Service {
	resolver := net.DefaultResolver
	return &Service{
		cfg:         cfg,
		store:       stateStore,
		secrets:     secretService,
		docker:      dockerSvc,
		caddy:       caddySvc,
		db:          dbengines.New(stateStore, secretService, dockerSvc),
		logger:      logger,
		lookupCNAME: resolver.LookupCNAME,
		lookupHost:  resolver.LookupHost,
		checkHTTP: func(ctx context.Context, target string, timeout time.Duration) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return fmt.Errorf("build health check request: %w", err)
			}
			client := &http.Client{Timeout: timeout}
			resp, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("request health check: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 400 {
				return fmt.Errorf("health check returned %d", resp.StatusCode)
			}
			return nil
		},
		deploySubs: map[string]map[chan DeployEvent]struct{}{},
		newCloudflare: func(token string) (cloudflareClient, error) {
			return cloudflare.New(token)
		},
	}
}

func (s *Service) SubscribeDeployEvents(projectID string) (<-chan DeployEvent, func()) {
	ch := make(chan DeployEvent, 16)

	s.deployMu.Lock()
	if s.deploySubs[projectID] == nil {
		s.deploySubs[projectID] = map[chan DeployEvent]struct{}{}
	}
	s.deploySubs[projectID][ch] = struct{}{}
	s.deployMu.Unlock()

	cancel := func() {
		s.deployMu.Lock()
		defer s.deployMu.Unlock()
		if subscribers, ok := s.deploySubs[projectID]; ok {
			delete(subscribers, ch)
			if len(subscribers) == 0 {
				delete(s.deploySubs, projectID)
			}
		}
		close(ch)
	}
	return ch, cancel
}

func (s *Service) emitDeployEvent(projectID, stage, message, status string) {
	event := DeployEvent{
		ProjectID: projectID,
		Stage:     stage,
		Message:   message,
		Status:    status,
		Timestamp: time.Now().UTC(),
	}

	s.deployMu.RLock()
	defer s.deployMu.RUnlock()
	for subscriber := range s.deploySubs[projectID] {
		select {
		case subscriber <- event:
		default:
		}
	}
}

func (s *Service) Dashboard(ctx context.Context) (Dashboard, error) {
	settings, err := s.loadSettings(ctx)
	if err != nil {
		return Dashboard{}, err
	}

	projects, err := s.loadProjects(ctx, settings)
	if err != nil {
		return Dashboard{}, err
	}

	return Dashboard{
		Settings: settings,
		Projects: projects,
	}, nil
}

func (s *Service) AuditLogs(ctx context.Context, filter string, limit int) ([]AuditLogEntry, error) {
	records, err := s.store.ListAuditLogs(ctx, filter, limit)
	if err != nil {
		return nil, err
	}
	entries := make([]AuditLogEntry, 0, len(records))
	for _, record := range records {
		entries = append(entries, AuditLogEntry{
			ID:        record.ID,
			Timestamp: record.Timestamp,
			UserEmail: record.UserEmail,
			Action:    record.Action,
			Target:    record.Target,
			Payload:   record.Payload,
		})
	}
	return entries, nil
}

func (s *Service) RuntimeSnapshot(ctx context.Context, project Project) (ProjectRuntimeSnapshot, error) {
	snapshot := ProjectRuntimeSnapshot{
		Available: true,
		Status:    project.Status,
	}
	if s.docker == nil {
		return snapshot, nil
	}
	if project.Status != "running" {
		snapshot.Warnings = append(snapshot.Warnings, "Container is not running, so the public URL may fail until the next healthy deploy.")
		return snapshot, nil
	}

	stats, err := s.docker.ContainerStats(ctx, project.ContainerName)
	if err != nil {
		return ProjectRuntimeSnapshot{}, err
	}

	snapshot.ReadAt = stats.ReadAt
	snapshot.CPUPercent = stats.CPUPercent
	snapshot.MemoryUsageBytes = stats.MemoryUsageBytes
	snapshot.MemoryLimitBytes = stats.MemoryLimitBytes
	snapshot.MemoryPercent = stats.MemoryPercent
	snapshot.NetworkRxBytes = stats.NetworkRxBytes
	snapshot.NetworkTxBytes = stats.NetworkTxBytes
	snapshot.BlockReadBytes = stats.BlockReadBytes
	snapshot.BlockWriteBytes = stats.BlockWriteBytes
	snapshot.PIDs = stats.PIDs
	snapshot.Warnings = runtimeWarnings(snapshot)
	return snapshot, nil
}

func (s *Service) GetProject(ctx context.Context, projectID string) (Project, Settings, error) {
	settings, err := s.loadSettings(ctx)
	if err != nil {
		return Project{}, Settings{}, err
	}

	record, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, Settings{}, err
	}

	project, err := s.projectFromRecord(ctx, record, settings)
	if err != nil {
		return Project{}, Settings{}, err
	}

	return project, settings, nil
}

func (s *Service) GetProjectBySlug(ctx context.Context, slug string) (Project, Settings, error) {
	settings, err := s.loadSettings(ctx)
	if err != nil {
		return Project{}, Settings{}, err
	}

	record, err := s.store.GetProjectBySlug(ctx, slug)
	if err != nil {
		return Project{}, Settings{}, err
	}

	project, err := s.projectFromRecord(ctx, record, settings)
	if err != nil {
		return Project{}, Settings{}, err
	}

	return project, settings, nil
}

func (s *Service) SaveSettings(ctx context.Context, input SettingsInput, userID string) error {
	previous, err := s.loadSettings(ctx)
	if err != nil {
		return err
	}

	values, err := s.store.GetSettings(ctx)
	if err != nil {
		return err
	}

	rootDomain := strings.TrimSpace(strings.ToLower(input.RootDomain))
	if rootDomain == "" {
		rootDomain = strings.TrimSpace(strings.ToLower(values[settingRootDomain]))
	}
	if rootDomain == "" {
		rootDomain = strings.TrimSpace(strings.ToLower(s.cfg.RootDomain))
	}
	originHostname := strings.TrimSpace(strings.ToLower(input.OriginHostname))
	zoneID := strings.TrimSpace(input.CloudflareZoneID)
	tokenValue := strings.TrimSpace(input.CloudflareToken)

	if tokenValue != "" {
		client, err := s.newCloudflare(tokenValue)
		if err != nil {
			return err
		}
		if err := client.ValidateToken(ctx); err != nil {
			return fmt.Errorf("validate cloudflare token: %w", err)
		}
		encoded, err := s.encodeSecret(tokenValue)
		if err != nil {
			return err
		}
		values[settingCloudflareAPIToken] = encoded
	}

	values[settingRootDomain] = rootDomain
	values[settingOriginHostname] = originHostname
	values[settingCloudflareZoneID] = zoneID
	values[settingCloudflareProxied] = strconv.FormatBool(input.CloudflareProxied)

	if err := s.store.UpsertSettings(ctx, values); err != nil {
		return err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "settings.update", "settings:deployment", map[string]any{
		"root_domain":        rootDomain,
		"origin_hostname":    originHostname,
		"cloudflare_zone_id": zoneID,
		"cloudflare_token":   tokenValue != "",
		"cloudflare_proxied": input.CloudflareProxied,
	}); err != nil {
		return err
	}
	if err := s.reconcileAdminAccess(ctx, previous); err != nil {
		return fmt.Errorf("settings saved but admin access update failed: %w", err)
	}
	return nil
}

func (s *Service) RestartController(ctx context.Context, userID string) error {
	if s.docker == nil {
		return fmt.Errorf("docker control unavailable")
	}
	if s.store != nil && userID != "" {
		if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "controller.restart", "controller:"+controllerContainerName, nil); err != nil {
			return err
		}
	}
	return s.docker.RestartContainer(ctx, controllerContainerName)
}

func (s *Service) CreateWebProject(ctx context.Context, input WebProjectInput, userID string) (Project, error) {
	input.Type = projectTypeWeb
	return s.CreateProject(ctx, input, userID)
}

func (s *Service) CreateProject(ctx context.Context, input WebProjectInput, userID string) (Project, error) {
	record, err := s.recordFromInput(input, "")
	if err != nil {
		return Project{}, err
	}

	if err := s.store.CreateProject(ctx, record); err != nil {
		return Project{}, err
	}

	project, settings, err := s.GetProject(ctx, record.ID)
	if err != nil {
		return Project{}, err
	}

	if !input.SkipDeploy {
		if err := s.applyProject(ctx, project, settings, projectDeployRequest{Trigger: "create", Actor: userID}); err != nil {
			return Project{}, fmt.Errorf("project saved but deployment failed: %w", err)
		}
		project, settings, err = s.GetProject(ctx, record.ID)
		if err != nil {
			return Project{}, err
		}
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.create", "project:"+record.ID, map[string]any{
		"slug":        record.Slug,
		"subdomain":   record.Subdomain,
		"skip_deploy": input.SkipDeploy,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) UpdateWebProject(ctx context.Context, input WebProjectInput, userID string) (Project, error) {
	input.Type = projectTypeWeb
	return s.UpdateProject(ctx, input, userID)
}

func (s *Service) UpdateProject(ctx context.Context, input WebProjectInput, userID string) (Project, error) {
	current, err := s.store.GetProject(ctx, input.ID)
	if err != nil {
		return Project{}, err
	}

	input.Type = current.Type
	record, err := s.recordFromInput(input, current.Slug)
	if err != nil {
		return Project{}, err
	}
	record.ID = current.ID
	record.Slug = current.Slug
	record.Type = current.Type
	record.WebhookSecret = current.WebhookSecret
	record.GitHubRepoFullName = current.GitHubRepoFullName
	record.GitHubInstallationID = current.GitHubInstallationID
	record.GitHubDefaultBranch = current.GitHubDefaultBranch

	if err := s.store.UpdateProject(ctx, record); err != nil {
		return Project{}, err
	}

	project, settings, err := s.GetProject(ctx, record.ID)
	if err != nil {
		return Project{}, err
	}

	if err := s.applyProject(ctx, project, settings, projectDeployRequest{Trigger: "update", Actor: userID}); err != nil {
		return Project{}, fmt.Errorf("project updated but deployment failed: %w", err)
	}
	project, _, err = s.GetProject(ctx, record.ID)
	if err != nil {
		return Project{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.update", "project:"+record.ID, map[string]any{
		"slug":      record.Slug,
		"subdomain": record.Subdomain,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) RedeployProject(ctx context.Context, projectID, userID string) (Project, error) {
	project, settings, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}

	if err := s.applyProject(ctx, project, settings, projectDeployRequest{Trigger: "redeploy", Actor: userID}); err != nil {
		return Project{}, err
	}
	project, _, err = s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.redeploy", "project:"+projectID, map[string]any{
		"slug": project.Slug,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) RedeployProjectByWebhook(ctx context.Context, slug string) (Project, error) {
	project, settings, err := s.GetProjectBySlug(ctx, slug)
	if err != nil {
		return Project{}, err
	}

	if err := s.applyProject(ctx, project, settings, projectDeployRequest{Trigger: "webhook", Actor: "webhook"}); err != nil {
		return Project{}, err
	}
	project, _, err = s.GetProject(ctx, project.ID)
	if err != nil {
		return Project{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), "", "project.webhook_redeploy", "project:"+project.ID, map[string]any{
		"slug": project.Slug,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) StreamProjectLogs(ctx context.Context, projectID string, tail int) (io.ReadCloser, error) {
	if s.docker == nil {
		return nil, fmt.Errorf("docker logs are unavailable")
	}

	project, _, err := s.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}

	return s.docker.StreamLogs(ctx, project.ContainerName, tail)
}

func (s *Service) DeleteProject(ctx context.Context, projectID, userID string) error {
	project, settings, err := s.GetProject(ctx, projectID)
	if err != nil {
		return err
	}

	if s.db != nil {
		for _, attachment := range project.DBAttachments {
			if err := s.db.DeleteAttachment(ctx, attachment.ID); err != nil {
				return err
			}
		}
	}

	if err := s.store.DeleteProject(ctx, projectID); err != nil {
		return err
	}

	if s.docker != nil {
		if err := s.docker.RemoveContainer(ctx, project.ContainerName); err != nil {
			return err
		}
	}

	if err := s.reconcileCaddy(ctx); err != nil {
		return err
	}

	if err := s.deleteCloudflare(ctx, project, settings); err != nil {
		return err
	}

	return s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.delete", "project:"+projectID, map[string]any{
		"slug": project.Slug,
	})
}

func (s *Service) AttachDatabase(ctx context.Context, input DatabaseAttachmentInput, userID string) (Project, error) {
	project, settings, err := s.GetProject(ctx, input.ProjectID)
	if err != nil {
		return Project{}, err
	}
	if s.db == nil {
		return Project{}, fmt.Errorf("database engine service is unavailable")
	}

	attachment, err := s.db.AttachDatabase(ctx, project.ID, project.Slug, input.Engine, input.EnvVarName)
	if err != nil {
		return Project{}, err
	}

	project, settings, err = s.GetProject(ctx, project.ID)
	if err != nil {
		return Project{}, err
	}
	if err := s.applyProject(ctx, project, settings, projectDeployRequest{Trigger: "db.attach", Actor: userID}); err != nil {
		return Project{}, fmt.Errorf("database attached but project redeploy failed: %w", err)
	}
	project, _, err = s.GetProject(ctx, project.ID)
	if err != nil {
		return Project{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.db.attach", "project:"+project.ID, map[string]any{
		"attachment": attachment.ID,
		"engine":     attachment.Engine,
		"env_var":    attachment.EnvVarName,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) RotateDatabaseAttachment(ctx context.Context, projectID string, attachmentID int64, userID string) (Project, error) {
	if s.db == nil {
		return Project{}, fmt.Errorf("database engine service is unavailable")
	}

	attachment, err := s.db.RotateAttachmentPassword(ctx, attachmentID)
	if err != nil {
		return Project{}, err
	}
	if attachment.ProjectID != projectID {
		return Project{}, fmt.Errorf("attachment does not belong to this project")
	}

	project, settings, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err := s.applyProject(ctx, project, settings, projectDeployRequest{Trigger: "db.rotate", Actor: userID}); err != nil {
		return Project{}, fmt.Errorf("credentials rotated but project redeploy failed: %w", err)
	}
	project, _, err = s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.db.rotate", "project:"+projectID, map[string]any{
		"attachment": attachmentID,
		"engine":     attachment.Engine,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) DeleteDatabaseAttachment(ctx context.Context, projectID string, attachmentID int64, userID string) (Project, error) {
	if s.db == nil {
		return Project{}, fmt.Errorf("database engine service is unavailable")
	}

	attachments, err := s.db.ListAttachments(ctx, projectID)
	if err != nil {
		return Project{}, err
	}

	found := false
	for _, attachment := range attachments {
		if attachment.ID == attachmentID {
			found = true
			break
		}
	}
	if !found {
		return Project{}, store.ErrNotFound
	}

	if err := s.db.DeleteAttachment(ctx, attachmentID); err != nil {
		return Project{}, err
	}

	project, settings, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err := s.applyProject(ctx, project, settings, projectDeployRequest{Trigger: "db.delete", Actor: userID}); err != nil {
		return Project{}, fmt.Errorf("database detached but project redeploy failed: %w", err)
	}
	project, _, err = s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.db.delete", "project:"+projectID, map[string]any{
		"attachment": attachmentID,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) AddProjectDomain(ctx context.Context, projectID, hostname string, makePrimary bool, userID string) (Project, error) {
	project, settings, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if project.Type != projectTypeWeb {
		return Project{}, fmt.Errorf("custom domains are only available for web projects")
	}

	hostname = normalizeHostname(hostname)
	if hostname == "" {
		return Project{}, fmt.Errorf("domain hostname is required")
	}
	if hostname == normalizeHostname(project.FullDomain) {
		return Project{}, fmt.Errorf("the generated project domain is already managed")
	}

	record, err := s.store.CreateProjectDomain(ctx, store.ProjectDomainRecord{
		ProjectID: projectID,
		Hostname:  hostname,
		IsPrimary: makePrimary,
	})
	if err != nil {
		return Project{}, err
	}

	project, settings, err = s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err := s.reconcileCaddy(ctx); err != nil {
		return Project{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.domain.add", "project:"+projectID, map[string]any{
		"domain_id":   record.ID,
		"hostname":    record.Hostname,
		"is_primary":  record.IsPrimary,
		"root_domain": settings.RootDomain,
	}); err != nil {
		return Project{}, err
	}
	return project, nil
}

func (s *Service) DeleteProjectDomain(ctx context.Context, projectID string, domainID int64, userID string) (Project, error) {
	domain, err := s.store.GetProjectDomain(ctx, projectID, domainID)
	if err != nil {
		return Project{}, err
	}
	if err := s.store.DeleteProjectDomain(ctx, projectID, domainID); err != nil {
		return Project{}, err
	}
	if err := s.reconcileCaddy(ctx); err != nil {
		return Project{}, err
	}
	project, _, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.domain.delete", "project:"+projectID, map[string]any{
		"domain_id": domainID,
		"hostname":  domain.Hostname,
	}); err != nil {
		return Project{}, err
	}
	return project, nil
}

func (s *Service) VerifyProjectDomain(ctx context.Context, projectID string, domainID int64, userID string) (Project, error) {
	project, settings, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	domain, err := s.store.GetProjectDomain(ctx, projectID, domainID)
	if err != nil {
		return Project{}, err
	}
	expected := normalizeHostname(settings.OriginHostname)
	if expected == "" {
		expected = normalizeHostname(project.FullDomain)
	}
	if expected == "" {
		return Project{}, fmt.Errorf("configure the origin hostname before verifying custom domains")
	}

	verified, message := s.verifyDomainTarget(ctx, domain.Hostname, expected)
	if !verified {
		return Project{}, fmt.Errorf("%s", message)
	}
	if err := s.store.MarkProjectDomainVerified(ctx, projectID, domainID, time.Now().UTC()); err != nil {
		return Project{}, err
	}
	project, _, err = s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.domain.verify", "project:"+projectID, map[string]any{
		"domain_id": domainID,
		"hostname":  domain.Hostname,
		"expected":  expected,
	}); err != nil {
		return Project{}, err
	}
	return project, nil
}

func (s *Service) RollbackProject(ctx context.Context, projectID string, deployID int64, userID string) (Project, error) {
	project, settings, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	deploy, err := s.store.GetProjectDeploy(ctx, projectID, deployID)
	if err != nil {
		return Project{}, err
	}
	imageRef := strings.TrimSpace(deploy.ImageDigest)
	if imageRef == "" {
		imageRef = strings.TrimSpace(deploy.ImageRef)
	}
	if imageRef == "" {
		return Project{}, fmt.Errorf("that deploy does not have a reusable image pin")
	}

	record, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	record.ImageRef = imageRef
	if err := s.store.UpdateProject(ctx, record); err != nil {
		return Project{}, err
	}

	project, settings, err = s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err := s.applyProject(ctx, project, settings, projectDeployRequest{Trigger: "rollback", Actor: userID}); err != nil {
		return Project{}, err
	}
	project, _, err = s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.rollback", "project:"+projectID, map[string]any{
		"deploy_id": deployID,
		"image_ref": deploy.ImageRef,
		"image_pin": imageRef,
	}); err != nil {
		return Project{}, err
	}
	return project, nil
}

func (s *Service) autoRollbackProject(ctx context.Context, projectID, imagePin string) error {
	record, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	record.ImageRef = strings.TrimSpace(imagePin)
	if err := s.store.UpdateProject(ctx, record); err != nil {
		return err
	}
	project, settings, err := s.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	return s.applyProject(ctx, project, settings, projectDeployRequest{
		Trigger:             "auto-rollback",
		Actor:               "system",
		DisableAutoRollback: true,
	})
}

func (s *Service) recordFromInput(input WebProjectInput, existingSlug string) (store.ProjectRecord, error) {
	projectType := strings.TrimSpace(strings.ToLower(input.Type))
	if projectType == "" {
		projectType = projectTypeWeb
	}
	if projectType != projectTypeWeb && projectType != projectTypeTCP && projectType != projectTypeUDP {
		return store.ProjectRecord{}, fmt.Errorf("project type must be web, tcp, or udp")
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		return store.ProjectRecord{}, fmt.Errorf("project name is required")
	}

	slug := existingSlug
	if slug == "" {
		slug = strings.TrimSpace(strings.ToLower(input.Slug))
	}
	if !slugPattern.MatchString(slug) {
		return store.ProjectRecord{}, fmt.Errorf("slug must match %s", slugPattern.String())
	}

	imageRef := strings.TrimSpace(input.ImageRef)
	if imageRef == "" {
		return store.ProjectRecord{}, fmt.Errorf("image reference is required")
	}

	subdomain := strings.TrimSpace(strings.ToLower(input.Subdomain))
	internalPort := 0
	var ports []store.ProjectPortRecord
	switch projectType {
	case projectTypeWeb:
		if subdomain == "" {
			return store.ProjectRecord{}, fmt.Errorf("subdomain is required")
		}
		if strings.Contains(subdomain, " ") || strings.HasPrefix(subdomain, ".") || strings.HasSuffix(subdomain, ".") {
			return store.ProjectRecord{}, fmt.Errorf("subdomain is invalid")
		}
		if input.InternalPort <= 0 || input.InternalPort > 65535 {
			return store.ProjectRecord{}, fmt.Errorf("internal port must be between 1 and 65535")
		}
		internalPort = input.InternalPort
		if healthPath := strings.TrimSpace(input.HealthCheckPath); healthPath != "" {
			if !strings.HasPrefix(healthPath, "/") {
				return store.ProjectRecord{}, fmt.Errorf("health check path must start with /")
			}
			if input.HealthCheckTimeoutSeconds < 0 || input.HealthCheckTimeoutSeconds > 30 {
				return store.ProjectRecord{}, fmt.Errorf("health check timeout must be between 1 and 30 seconds")
			}
		}
	case projectTypeTCP, projectTypeUDP:
		parsedPorts, err := parsePortMappings(input.PortMappingsText, projectType)
		if err != nil {
			return store.ProjectRecord{}, err
		}
		ports = make([]store.ProjectPortRecord, 0, len(parsedPorts))
		for _, port := range parsedPorts {
			ports = append(ports, store.ProjectPortRecord{
				Proto:         port.Proto,
				HostPort:      port.HostPort,
				ContainerPort: port.ContainerPort,
			})
		}
	}

	env, err := s.encodeEnv(parseEnvText(input.EnvText))
	if err != nil {
		return store.ProjectRecord{}, err
	}

	id := input.ID
	if id == "" {
		id = uuid.NewString()
	}

	return store.ProjectRecord{
		ID:                        id,
		Slug:                      slug,
		Name:                      name,
		Type:                      projectType,
		ImageRef:                  imageRef,
		GitHubRepoFullName:        strings.TrimSpace(input.GitHubRepoFullName),
		GitHubInstallationID:      input.GitHubInstallationID,
		GitHubDefaultBranch:       strings.TrimSpace(input.GitHubDefaultBranch),
		InternalPort:              internalPort,
		Subdomain:                 subdomain,
		WatchtowerEnabled:         input.WatchtowerEnabled,
		WebhookSecret:             randomSecret(),
		Env:                       env,
		Ports:                     ports,
		HealthCheckPath:           strings.TrimSpace(input.HealthCheckPath),
		HealthCheckTimeoutSeconds: normalizedHealthTimeout(strings.TrimSpace(input.HealthCheckPath), input.HealthCheckTimeoutSeconds),
	}, nil
}

func (s *Service) loadSettings(ctx context.Context) (Settings, error) {
	raw, err := s.store.GetSettings(ctx)
	if err != nil {
		return Settings{}, err
	}

	rootDomain := strings.TrimSpace(raw[settingRootDomain])
	if rootDomain == "" {
		rootDomain = strings.TrimSpace(s.cfg.RootDomain)
		if rootDomain != "" {
			raw[settingRootDomain] = rootDomain
			if err := s.store.UpsertSettings(ctx, raw); err != nil {
				return Settings{}, err
			}
		}
	}

	return Settings{
		RootDomain:             rootDomain,
		OriginHostname:         strings.TrimSpace(raw[settingOriginHostname]),
		CloudflareZoneID:       strings.TrimSpace(raw[settingCloudflareZoneID]),
		CloudflareProxied:      strings.EqualFold(strings.TrimSpace(raw[settingCloudflareProxied]), "true"),
		CloudflareTokenPresent: strings.TrimSpace(raw[settingCloudflareAPIToken]) != "",
	}, nil
}

func (s *Service) loadProjects(ctx context.Context, settings Settings) ([]Project, error) {
	records, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}

	projects := make([]Project, 0, len(records))
	for _, record := range records {
		project, err := s.projectFromRecord(ctx, record, settings)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}

	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Name < projects[j].Name
	})

	return projects, nil
}

func (s *Service) projectFromRecord(ctx context.Context, record store.ProjectRecord, settings Settings) (Project, error) {
	env, err := s.decodeEnv(record.Env)
	if err != nil {
		return Project{}, err
	}
	attachments, err := s.loadAttachments(ctx, record.ID)
	if err != nil {
		return Project{}, err
	}
	domains, err := s.loadProjectDomains(ctx, record.ID)
	if err != nil {
		return Project{}, err
	}
	deploys, err := s.loadProjectDeploys(ctx, record.ID, 10)
	if err != nil {
		return Project{}, err
	}

	status := "not deployed"
	if s.docker != nil {
		if inspect, err := s.docker.InspectContainer(ctx, containerName(record.Slug)); err == nil {
			if inspect.Running {
				status = "running"
			} else {
				status = "stopped"
			}
		}
	}
	if status == "not deployed" && strings.TrimSpace(record.GitHubRepoFullName) != "" {
		status = "pending image"
	}
	primaryDomain := fqdn(settings.RootDomain, record.Subdomain)
	for _, domain := range domains {
		if domain.IsPrimary {
			primaryDomain = domain.Hostname
			break
		}
	}

	return Project{
		ID:                        record.ID,
		Name:                      record.Name,
		Slug:                      record.Slug,
		Type:                      record.Type,
		WebhookSecret:             record.WebhookSecret,
		ImageRef:                  record.ImageRef,
		GitHubRepoFullName:        record.GitHubRepoFullName,
		GitHubInstallationID:      record.GitHubInstallationID,
		GitHubDefaultBranch:       record.GitHubDefaultBranch,
		Subdomain:                 record.Subdomain,
		FullDomain:                fqdn(settings.RootDomain, record.Subdomain),
		ContainerName:             containerName(record.Slug),
		InternalPort:              record.InternalPort,
		Ports:                     projectPortsFromStore(record.Ports),
		WatchtowerEnabled:         record.WatchtowerEnabled,
		Env:                       env,
		EnvText:                   envText(env),
		HealthCheckPath:           record.HealthCheckPath,
		HealthCheckTimeoutSeconds: record.HealthCheckTimeoutSeconds,
		CreatedAt:                 record.CreatedAt,
		UpdatedAt:                 record.UpdatedAt,
		Status:                    status,
		PrimaryDomain:             primaryDomain,
		CustomDomains:             domains,
		Deploys:                   deploys,
		DBAttachments:             attachments,
	}, nil
}

func (s *Service) loadProjectDomains(ctx context.Context, projectID string) ([]ProjectDomain, error) {
	records, err := s.store.ListProjectDomains(ctx, projectID)
	if err != nil {
		return nil, err
	}
	domains := make([]ProjectDomain, 0, len(records))
	for _, record := range records {
		domains = append(domains, ProjectDomain{
			ID:            record.ID,
			Hostname:      record.Hostname,
			IsPrimary:     record.IsPrimary,
			DNSVerifiedAt: record.DNSVerifiedAt,
		})
	}
	return domains, nil
}

func (s *Service) loadProjectDeploys(ctx context.Context, projectID string, limit int) ([]ProjectDeploy, error) {
	records, err := s.store.ListProjectDeploys(ctx, projectID, limit)
	if err != nil {
		return nil, err
	}
	deploys := make([]ProjectDeploy, 0, len(records))
	for _, record := range records {
		deploys = append(deploys, ProjectDeploy{
			ID:          record.ID,
			ImageDigest: record.ImageDigest,
			ImageRef:    record.ImageRef,
			Status:      record.Status,
			Trigger:     record.Trigger,
			Actor:       record.Actor,
			StartedAt:   record.StartedAt,
			FinishedAt:  record.FinishedAt,
			Error:       record.Error,
		})
	}
	return deploys, nil
}

func (s *Service) applyProject(ctx context.Context, project Project, settings Settings, request projectDeployRequest) error {
	if project.Type == projectTypeWeb && settings.RootDomain == "" {
		return fmt.Errorf("configure the root domain before deploying projects")
	}

	deploy, err := s.store.StartProjectDeploy(ctx, store.ProjectDeployRecord{
		ProjectID: project.ID,
		ImageRef:  project.ImageRef,
		Status:    "running",
		Trigger:   fallbackString(request.Trigger, "deploy"),
		Actor:     strings.TrimSpace(request.Actor),
	})
	if err != nil {
		return err
	}

	finalize := func(status, imageDigest, errorMessage string) error {
		if finishErr := s.store.FinishProjectDeploy(ctx, project.ID, deploy.ID, status, imageDigest, errorMessage); finishErr != nil {
			return finishErr
		}
		return nil
	}

	imageDigest := ""
	previousHealthyPin := ""
	if !request.DisableAutoRollback {
		if previous, err := s.store.ListProjectDeploys(ctx, project.ID, 20); err == nil {
			for _, item := range previous {
				if item.ID == deploy.ID {
					continue
				}
				if item.Status == "live" && strings.TrimSpace(item.ImageDigest) != "" {
					previousHealthyPin = item.ImageDigest
					break
				}
			}
		}
	}
	s.emitDeployEvent(project.ID, "queued", "Starting deployment workflow.", "running")

	if s.docker != nil {
		if shouldPullImage(project.ImageRef) {
			s.emitDeployEvent(project.ID, "pull", "Pulling image from registry.", "running")
			if err := s.docker.PullImage(ctx, project.ImageRef); err != nil {
				s.emitDeployEvent(project.ID, "failed", err.Error(), "failed")
				if finishErr := finalize("failed", imageDigest, err.Error()); finishErr != nil {
					return finishErr
				}
				return err
			}
		} else {
			s.emitDeployEvent(project.ID, "pull", "Using a pinned local image for rollback.", "running")
		}

		labels := map[string]string{
			"caddytower.managed": "true",
			"caddytower.project": project.Slug,
			"caddytower.type":    project.Type,
		}
		if project.WatchtowerEnabled {
			labels["com.centurylinklabs.watchtower.enable"] = "true"
		}

		spec := dockerx.ContainerSpec{
			Name:          project.ContainerName,
			Image:         project.ImageRef,
			Env:           project.runtimeEnv(),
			Labels:        labels,
			Network:       managedNetworkName,
			RestartPolicy: "unless-stopped",
		}
		if project.Type == projectTypeWeb {
			spec.ExposedPorts = []string{strconv.Itoa(project.InternalPort)}
		} else {
			spec.PublishedPorts = publishedPorts(project.Ports)
		}

		s.emitDeployEvent(project.ID, "container", "Recreating the container.", "running")
		inspect, err := s.docker.RecreateContainer(ctx, spec)
		if err != nil {
			s.emitDeployEvent(project.ID, "failed", err.Error(), "failed")
			if finishErr := finalize("failed", imageDigest, err.Error()); finishErr != nil {
				return finishErr
			}
			return err
		}
		imageDigest = inspect.ImageID
	}

	if project.Type == projectTypeWeb && strings.TrimSpace(project.HealthCheckPath) != "" {
		targetURL := s.projectHealthURL(project)
		s.emitDeployEvent(project.ID, "health-check", "Running health checks against "+targetURL+".", "running")
		if err := s.waitForProjectHealthy(ctx, project, targetURL); err != nil {
			s.emitDeployEvent(project.ID, "failed", err.Error(), "failed")
			if finishErr := finalize("failed", imageDigest, err.Error()); finishErr != nil {
				return finishErr
			}
			if previousHealthyPin != "" && !request.DisableAutoRollback {
				s.emitDeployEvent(project.ID, "rollback", "Health checks failed. Rolling back to the last healthy image.", "running")
				if rollbackErr := s.autoRollbackProject(ctx, project.ID, previousHealthyPin); rollbackErr != nil {
					return fmt.Errorf("%s; automatic rollback failed: %w", err.Error(), rollbackErr)
				}
				return fmt.Errorf("%s; automatically rolled back to the last healthy image", err.Error())
			}
			return err
		}
		s.emitDeployEvent(project.ID, "health-check", "Health check passed.", "live")
	}

	s.emitDeployEvent(project.ID, "routing", "Reconciling Caddy routes.", "running")
	if err := s.reconcileCaddy(ctx); err != nil {
		s.emitDeployEvent(project.ID, "failed", err.Error(), "failed")
		if finishErr := finalize("failed", imageDigest, err.Error()); finishErr != nil {
			return finishErr
		}
		return err
	}

	s.emitDeployEvent(project.ID, "dns", "Updating managed DNS records.", "running")
	if err := s.upsertCloudflare(ctx, project, settings); err != nil {
		s.emitDeployEvent(project.ID, "failed", err.Error(), "failed")
		if finishErr := finalize("failed", imageDigest, err.Error()); finishErr != nil {
			return finishErr
		}
		return err
	}

	s.emitDeployEvent(project.ID, "live", "Deployment is live.", "live")
	if err := finalize("live", imageDigest, ""); err != nil {
		return err
	}
	return nil
}

func (p Project) runtimeEnv() map[string]string {
	values := make(map[string]string, len(p.Env)+len(p.DBAttachments))
	for key, value := range p.Env {
		values[key] = value
	}
	for _, attachment := range p.DBAttachments {
		values[attachment.EnvVarName] = attachment.ConnectionHint
	}
	return values
}

func (s *Service) reconcileCaddy(ctx context.Context) error {
	if s.caddy == nil {
		return nil
	}

	settings, err := s.loadSettings(ctx)
	if err != nil {
		return err
	}

	records, err := s.store.ListProjects(ctx)
	if err != nil {
		return err
	}

	routes := make([]caddyadmin.HTTPRoute, 0, len(records))
	managedHosts := make([]string, 0, len(records))
	if settings.RootDomain != "" {
		adminHost := adminHostname(settings.RootDomain)
		routes = append(routes, caddyadmin.HTTPRoute{
			Host:      adminHost,
			Upstreams: []string{controllerContainerName + ":" + controllerPort(s.cfg.HTTPAddr)},
		})
		managedHosts = append(managedHosts, adminHost)
	}
	for _, record := range records {
		if record.Type != projectTypeWeb {
			continue
		}
		hosts := []string{fqdn(settings.RootDomain, record.Subdomain)}
		domains, err := s.store.ListProjectDomains(ctx, record.ID)
		if err != nil {
			return err
		}
		for _, domain := range domains {
			hosts = append(hosts, domain.Hostname)
		}
		for _, host := range hosts {
			host = strings.TrimSpace(host)
			if host == "" {
				continue
			}
			routes = append(routes, caddyadmin.HTTPRoute{
				Host:      host,
				Upstreams: []string{containerName(record.Slug) + ":" + strconv.Itoa(record.InternalPort)},
			})
			managedHosts = append(managedHosts, host)
		}
	}
	if len(managedHosts) == 0 {
		return nil
	}
	if settings.RootDomain == "" {
		return fmt.Errorf("root domain is required for caddy reconciliation")
	}

	_, err = s.caddy.ReconcileManagedRoutes(ctx, routes, managedHosts)
	return err
}

func (s *Service) upsertCloudflare(ctx context.Context, project Project, settings Settings) error {
	if project.Type != projectTypeWeb || project.FullDomain == "" {
		return nil
	}
	if settings.CloudflareZoneID == "" || settings.OriginHostname == "" {
		return nil
	}

	token, err := s.cloudflareToken(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}

	client, err := s.newCloudflare(token)
	if err != nil {
		return err
	}

	_, _, err = client.UpsertRecord(ctx, settings.CloudflareZoneID, project.FullDomain, settings.OriginHostname, settings.CloudflareProxied)
	return err
}

func (s *Service) deleteCloudflare(ctx context.Context, project Project, settings Settings) error {
	if project.Type != projectTypeWeb || project.FullDomain == "" {
		return nil
	}
	if settings.CloudflareZoneID == "" {
		return nil
	}

	token, err := s.cloudflareToken(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}

	client, err := s.newCloudflare(token)
	if err != nil {
		return err
	}

	return client.DeleteRecord(ctx, settings.CloudflareZoneID, project.FullDomain)
}

func (s *Service) cloudflareToken(ctx context.Context) (string, error) {
	settings, err := s.store.GetSettings(ctx, settingCloudflareAPIToken)
	if err != nil {
		return "", err
	}
	return s.decodeSecret(settings[settingCloudflareAPIToken])
}

func (s *Service) encodeEnv(values map[string]string) (map[string]string, error) {
	encoded := make(map[string]string, len(values))
	for key, value := range values {
		secret, err := s.encodeSecret(value)
		if err != nil {
			return nil, err
		}
		encoded[key] = secret
	}
	return encoded, nil
}

func (s *Service) decodeEnv(values map[string]string) (map[string]string, error) {
	decoded := make(map[string]string, len(values))
	for key, value := range values {
		secret, err := s.decodeSecret(value)
		if err != nil {
			return nil, err
		}
		decoded[key] = secret
	}
	return decoded, nil
}

func (s *Service) encodeSecret(value string) (string, error) {
	if s.secrets == nil || value == "" {
		return value, nil
	}
	encrypted, err := s.secrets.EncryptString(value)
	if err != nil {
		return "", err
	}
	return "enc:" + encrypted, nil
}

func (s *Service) decodeSecret(value string) (string, error) {
	if !strings.HasPrefix(value, "enc:") {
		return value, nil
	}
	if s.secrets == nil {
		return "", fmt.Errorf("encrypted value present but master key is unavailable")
	}
	return s.secrets.DecryptString(strings.TrimPrefix(value, "enc:"))
}

func (s *Service) loadAttachments(ctx context.Context, projectID string) ([]DBAttachment, error) {
	if s.db == nil {
		return nil, nil
	}

	attachments, err := s.db.ListAttachments(ctx, projectID)
	if err != nil {
		return nil, err
	}

	result := make([]DBAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		result = append(result, DBAttachment{
			ID:             attachment.ID,
			Engine:         attachment.Engine,
			DBName:         attachment.DBName,
			DBUser:         attachment.DBUser,
			EnvVarName:     attachment.EnvVarName,
			Host:           attachment.Host,
			Port:           attachment.Port,
			Password:       attachment.Password,
			ConnectionHint: connectionString(attachment),
		})
	}

	return result, nil
}

func projectPortsFromStore(records []store.ProjectPortRecord) []ProjectPort {
	if len(records) == 0 {
		return nil
	}

	ports := make([]ProjectPort, 0, len(records))
	for _, record := range records {
		ports = append(ports, ProjectPort{
			Proto:         record.Proto,
			HostPort:      record.HostPort,
			ContainerPort: record.ContainerPort,
		})
	}
	return ports
}

func publishedPorts(ports []ProjectPort) []dockerx.PortBinding {
	if len(ports) == 0 {
		return nil
	}

	bindings := make([]dockerx.PortBinding, 0, len(ports))
	for _, port := range ports {
		bindings = append(bindings, dockerx.PortBinding{
			ContainerPort: strconv.Itoa(port.ContainerPort),
			HostPort:      strconv.Itoa(port.HostPort),
			Protocol:      port.Proto,
		})
	}
	return bindings
}

func PortMappingsText(ports []ProjectPort) string {
	if len(ports) == 0 {
		return ""
	}

	lines := make([]string, 0, len(ports))
	for _, port := range ports {
		lines = append(lines, fmt.Sprintf("%d:%d", port.HostPort, port.ContainerPort))
	}
	return strings.Join(lines, "\n")
}

func parsePortMappings(raw, proto string) ([]ProjectPort, error) {
	lines := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	})
	if len(lines) == 0 {
		return nil, fmt.Errorf("at least one port mapping is required")
	}

	seen := map[string]struct{}{}
	seenHostPorts := map[int]struct{}{}
	ports := make([]ProjectPort, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid port mapping %q: use host:container", line)
		}
		hostPort, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || hostPort <= 0 || hostPort > 65535 {
			return nil, fmt.Errorf("invalid host port in %q", line)
		}
		containerPort, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || containerPort <= 0 || containerPort > 65535 {
			return nil, fmt.Errorf("invalid container port in %q", line)
		}
		key := fmt.Sprintf("%s/%d/%d", proto, hostPort, containerPort)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate port mapping %q", line)
		}
		if _, ok := seenHostPorts[hostPort]; ok {
			return nil, fmt.Errorf("host port %d is listed more than once", hostPort)
		}
		seen[key] = struct{}{}
		seenHostPorts[hostPort] = struct{}{}
		ports = append(ports, ProjectPort{
			Proto:         proto,
			HostPort:      hostPort,
			ContainerPort: containerPort,
		})
	}

	if len(ports) == 0 {
		return nil, fmt.Errorf("at least one port mapping is required")
	}

	sort.Slice(ports, func(i, j int) bool {
		if ports[i].HostPort != ports[j].HostPort {
			return ports[i].HostPort < ports[j].HostPort
		}
		return ports[i].ContainerPort < ports[j].ContainerPort
	})
	return ports, nil
}

func parseEnvText(raw string) map[string]string {
	values := map[string]string{}
	lines := strings.Split(raw, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		value := ""
		if len(parts) == 2 {
			value = strings.TrimSpace(parts[1])
		}
		if key != "" {
			values[key] = value
		}
	}
	return values
}

func envText(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+values[key])
	}
	return strings.Join(lines, "\n")
}

func fqdn(rootDomain, subdomain string) string {
	if strings.TrimSpace(subdomain) == "" {
		return ""
	}
	if rootDomain == "" {
		return subdomain
	}
	return strings.TrimSuffix(subdomain, ".") + "." + strings.TrimPrefix(rootDomain, ".")
}

func normalizeHostname(value string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(value)), ".")
}

func normalizedHealthTimeout(path string, timeoutSeconds int) int {
	if strings.TrimSpace(path) == "" {
		return 0
	}
	if timeoutSeconds <= 0 {
		return 5
	}
	return timeoutSeconds
}

func shouldPullImage(imageRef string) bool {
	return !strings.HasPrefix(strings.TrimSpace(imageRef), "sha256:")
}

func fallbackString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func (s *Service) verifyDomainTarget(ctx context.Context, hostname, expected string) (bool, string) {
	hostname = normalizeHostname(hostname)
	expected = normalizeHostname(expected)

	if cname, err := s.lookupCNAME(ctx, hostname); err == nil {
		if normalizeHostname(cname) == expected {
			return true, "DNS verified"
		}
	}

	hostAddrs, hostErr := s.lookupHost(ctx, hostname)
	expectedAddrs, expectedErr := s.lookupHost(ctx, expected)
	if hostErr == nil && expectedErr == nil {
		expectedSet := map[string]struct{}{}
		for _, addr := range expectedAddrs {
			expectedSet[strings.TrimSpace(addr)] = struct{}{}
		}
		for _, addr := range hostAddrs {
			if _, ok := expectedSet[strings.TrimSpace(addr)]; ok {
				return true, "DNS verified"
			}
		}
	}

	return false, fmt.Sprintf("DNS for %s does not point to %s yet", hostname, expected)
}

func (s *Service) projectHealthURL(project Project) string {
	base := fmt.Sprintf("http://%s:%d", project.ContainerName, project.InternalPort)
	path := strings.TrimSpace(project.HealthCheckPath)
	if path == "" {
		return base
	}
	return strings.TrimRight(base, "/") + path
}

func (s *Service) waitForProjectHealthy(ctx context.Context, project Project, targetURL string) error {
	timeout := time.Duration(normalizedHealthTimeout(project.HealthCheckPath, project.HealthCheckTimeoutSeconds)) * time.Second
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
		if err := s.checkHTTP(ctx, targetURL, timeout); err == nil {
			return nil
		} else {
			lastErr = err
			s.emitDeployEvent(project.ID, "health-check", fmt.Sprintf("Attempt %d/3 failed: %s", attempt, err.Error()), "running")
		}
	}
	return fmt.Errorf("health check failed after 3 attempts: %w", lastErr)
}

func runtimeWarnings(snapshot ProjectRuntimeSnapshot) []string {
	warnings := []string{}
	if snapshot.Status != "running" {
		warnings = append(warnings, "Container is not running, so the public URL may fail until the next healthy deploy.")
	}
	if snapshot.MemoryLimitBytes > 0 && snapshot.MemoryPercent >= 85 {
		warnings = append(warnings, "Memory usage is close to the container limit. Consider a leaner image or lower concurrency.")
	}
	if snapshot.CPUPercent >= 90 {
		warnings = append(warnings, "CPU usage is very high right now. If requests feel slow, check the app logs for load or hot loops.")
	}
	return warnings
}

func containerName(slug string) string {
	return "caddytower-" + slug
}

func randomSecret() string {
	return uuid.NewString() + uuid.NewString()
}

func ParseBoolCheckbox(raw string) bool {
	return raw == "on" || raw == "true" || raw == "1"
}

func HostFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func connectionString(attachment dbengines.Attachment) string {
	switch attachment.Engine {
	case "mariadb":
		return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true", attachment.DBUser, attachment.Password, attachment.Host, attachment.Port, attachment.DBName)
	default:
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", attachment.DBUser, attachment.Password, attachment.Host, attachment.Port, attachment.DBName)
	}
}

func (s *Service) reconcileAdminAccess(ctx context.Context, previous Settings) error {
	current, err := s.loadSettings(ctx)
	if err != nil {
		return err
	}
	if err := s.reconcileCaddy(ctx); err != nil {
		return err
	}
	if previous.RootDomain != "" && previous.RootDomain != current.RootDomain {
		if err := s.deleteAdminCloudflare(ctx, previous); err != nil {
			return err
		}
	}
	return s.upsertAdminCloudflare(ctx, current)
}

func (s *Service) upsertAdminCloudflare(ctx context.Context, settings Settings) error {
	adminHost := adminHostname(settings.RootDomain)
	if adminHost == "" || strings.TrimSpace(settings.OriginHostname) == "" || strings.TrimSpace(settings.CloudflareZoneID) == "" {
		return nil
	}

	token, err := s.cloudflareToken(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}

	client, err := s.newCloudflare(token)
	if err != nil {
		return err
	}

	_, _, err = client.UpsertRecord(ctx, settings.CloudflareZoneID, adminHost, settings.OriginHostname, settings.CloudflareProxied)
	return err
}

func (s *Service) deleteAdminCloudflare(ctx context.Context, settings Settings) error {
	adminHost := adminHostname(settings.RootDomain)
	if adminHost == "" || strings.TrimSpace(settings.CloudflareZoneID) == "" {
		return nil
	}

	token, err := s.cloudflareToken(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}

	client, err := s.newCloudflare(token)
	if err != nil {
		return err
	}

	return client.DeleteRecord(ctx, settings.CloudflareZoneID, adminHost)
}

func adminHostname(rootDomain string) string {
	rootDomain = strings.TrimSpace(rootDomain)
	if rootDomain == "" {
		return ""
	}
	return controllerSubdomain + "." + strings.TrimPrefix(rootDomain, ".")
}

func controllerPort(httpAddr string) string {
	httpAddr = strings.TrimSpace(httpAddr)
	if httpAddr == "" {
		return "8080"
	}
	if strings.HasPrefix(httpAddr, ":") {
		return strings.TrimPrefix(httpAddr, ":")
	}
	if _, port, err := net.SplitHostPort(httpAddr); err == nil && strings.TrimSpace(port) != "" {
		return port
	}
	return "8080"
}
