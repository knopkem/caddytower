package projects

import (
	"bytes"
	"context"
	"encoding/json"
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
	githubapp "caddytower/internal/github"
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
	settingGitHubAppID        = "github_app_id"
	settingGitHubAppSlug      = "github_app_slug"
	settingGitHubWebhook      = "github_webhook_secret"
	settingGitHubPrivateKey   = "github_private_key_pem"
	managedNetworkName        = "edge"
	controllerSubdomain       = "caddytower"
	controllerContainerName   = "caddytower"
	projectTypeWeb            = "web"
	projectTypeTCP            = "tcp"
	projectTypeUDP            = "udp"
	mountTypeBind             = "bind"
	httpRouteMatchHost        = "host"
	httpRouteMatchPathPrefix  = "path_prefix"
	httpRouteMatchPathExact   = "path_exact"
	httpRouteAllDomainsScope  = "@domains"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

type dockerService interface {
	Ping(context.Context) error
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
	Ping(context.Context) error
	GetConfig(context.Context) (json.RawMessage, error)
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

type GitHubSettings struct {
	AppID                int64
	AppSlug              string
	WebhookSecret        string
	PrivateKeyPEM        string
	PrivateKeyPath       string
	WebhookSecretPresent bool
	PrivateKeyPresent    bool
	Configured           bool
	StoredInApp          bool
}

type GitHubSettingsInput struct {
	AppID         int64
	AppSlug       string
	WebhookSecret string
	PrivateKeyPEM string
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
	Mounts                    []ProjectMount
	WatchtowerEnabled         bool
	Env                       map[string]string
	EnvText                   string
	MountsText                string
	HTTPRoutes                []ProjectHTTPRoute
	HTTPRoutesText            string
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

type ProjectMount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

type ProjectHTTPRoute struct {
	Hostname      string
	MatchType     string
	MatchValue    string
	StripPrefix   bool
	RewritePrefix string
	Priority      int
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
	Settings     Settings
	Projects     []Project
	Requirements RequirementsStatus
	Caddy        CaddyDiagnostics
}

type RequirementsStatus struct {
	Available    bool
	HealthyCount int
	WarningCount int
	FailureCount int
	Checks       []RequirementCheck
}

type RequirementCheck struct {
	Name    string
	Status  string
	Summary string
	Detail  string
}

type CaddyDiagnostics struct {
	Available          bool
	Status             string
	Summary            string
	Detail             string
	ManagedRouteCount  int
	HealthyRouteCount  int
	DriftCount         int
	LiveRouteCount     int
	Routes             []CaddyRouteDiagnostic
	RawConfig          string
	RawConfigAvailable bool
	RawConfigError     string
}

type CaddyRouteDiagnostic struct {
	Host              string
	MatcherSummary    string
	ExpectedUpstreams []string
	LiveUpstreams     []string
	Status            string
	Detail            string
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
	MountsText                string
	HTTPRoutesText            string
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
		Settings:     settings,
		Projects:     projects,
		Requirements: s.requirementsStatus(ctx, projects),
		Caddy:        s.caddyDiagnostics(ctx, settings, projects),
	}, nil
}

func (s *Service) requirementsStatus(ctx context.Context, projectList []Project) RequirementsStatus {
	checks := make([]RequirementCheck, 0, 3)
	dockerCheck := s.dockerRequirement(ctx)
	checks = append(checks, dockerCheck)
	checks = append(checks, s.caddyRequirement(ctx))
	checks = append(checks, s.watchtowerRequirement(ctx, projectList, dockerCheck.Status == "ok"))

	status := RequirementsStatus{
		Available: true,
		Checks:    checks,
	}
	for _, check := range checks {
		switch check.Status {
		case "ok":
			status.HealthyCount++
		case "warning":
			status.WarningCount++
		case "error":
			status.FailureCount++
		}
	}
	return status
}

func (s *Service) dockerRequirement(ctx context.Context) RequirementCheck {
	check := RequirementCheck{
		Name:   "Docker daemon",
		Status: "error",
	}
	if s.docker == nil {
		check.Summary = "Docker integration is not configured."
		check.Detail = "CaddyTower needs Docker access for deploys, logs, restarts, and runtime inspection."
		return check
	}

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.docker.Ping(checkCtx); err != nil {
		check.Summary = "CaddyTower cannot reach Docker right now."
		check.Detail = dockerRequirementDetail(err)
		return check
	}

	check.Status = "ok"
	check.Summary = "Docker is reachable for deploys, logs, and container control."
	check.Detail = "Project lifecycle actions can talk to the Docker daemon."
	return check
}

func (s *Service) caddyRequirement(ctx context.Context) RequirementCheck {
	check := RequirementCheck{
		Name:   "Shared Caddy admin API",
		Status: "error",
	}
	adminURL := strings.TrimSpace(s.cfg.CaddyAdminURL)
	if adminURL == "" {
		check.Summary = "The Caddy admin URL is not configured."
		check.Detail = "Set CADDYTOWER_CADDY_ADMIN_URL so CaddyTower can update shared routes."
		return check
	}
	if s.caddy == nil {
		check.Summary = "Caddy admin integration is not configured."
		check.Detail = "CaddyTower needs the shared Caddy admin API to publish and update routes."
		return check
	}

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.caddy.Ping(checkCtx); err != nil {
		check.Summary = "CaddyTower cannot reach the shared Caddy admin API."
		check.Detail = err.Error()
		return check
	}

	check.Status = "ok"
	check.Summary = "Shared Caddy routing is reachable for route reconciliation."
	check.Detail = adminURL
	return check
}

func (s *Service) watchtowerRequirement(ctx context.Context, projectList []Project, dockerHealthy bool) RequirementCheck {
	autoUpdateProjects := 0
	for _, project := range projectList {
		if project.WatchtowerEnabled {
			autoUpdateProjects++
		}
	}

	check := RequirementCheck{
		Name:   "Watchtower auto-updater",
		Status: "warning",
	}
	if s.docker == nil {
		check.Summary = "Watchtower could not be checked because Docker integration is missing."
		check.Detail = "Automatic image refresh depends on Docker access."
		return check
	}
	if !dockerHealthy {
		check.Summary = "Watchtower could not be checked because Docker is unavailable."
		check.Detail = "Restore Docker access first, then re-check the updater container."
		return check
	}

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	inspect, err := s.docker.InspectContainer(checkCtx, "watchtower")
	if err != nil {
		if autoUpdateProjects > 0 {
			check.Summary = fmt.Sprintf("Watchtower is missing while %d project%s use automatic image updates.", autoUpdateProjects, plural(autoUpdateProjects))
		} else {
			check.Summary = "Watchtower is not running."
		}
		check.Detail = "The dashboard still works, but automatic image refresh is unavailable until the watchtower container is recreated."
		return check
	}
	if !inspect.Running {
		check.Summary = "Watchtower exists but is not running."
		check.Detail = "Start or recreate the watchtower container so automatic image refresh can resume."
		return check
	}
	if !envContainsPrefix(inspect.Env, "DOCKER_API_VERSION=") {
		check.Summary = "Watchtower is running, but Docker API compatibility is not pinned."
		check.Detail = "Set DOCKER_API_VERSION on the watchtower container to avoid restart loops on newer Docker daemons."
		return check
	}

	check.Status = "ok"
	if autoUpdateProjects > 0 {
		check.Summary = fmt.Sprintf("Watchtower is running for %d auto-update project%s.", autoUpdateProjects, plural(autoUpdateProjects))
	} else {
		check.Summary = "Watchtower is running and ready for projects that opt into automatic image refresh."
	}
	check.Detail = "Container name: watchtower"
	return check
}

func (s *Service) caddyDiagnostics(ctx context.Context, settings Settings, projectList []Project) CaddyDiagnostics {
	diagnostics := CaddyDiagnostics{
		Available: false,
		Status:    "error",
	}
	adminURL := strings.TrimSpace(s.cfg.CaddyAdminURL)
	if adminURL == "" {
		diagnostics.Summary = "Shared Caddy diagnostics are unavailable because the admin URL is not configured."
		diagnostics.Detail = "Set CADDYTOWER_CADDY_ADMIN_URL so CaddyTower can inspect and reconcile shared routes."
		return diagnostics
	}
	if s.caddy == nil {
		diagnostics.Summary = "Shared Caddy diagnostics are unavailable because the admin client is not configured."
		diagnostics.Detail = "CaddyTower needs the shared Caddy admin API to inspect live routes."
		return diagnostics
	}

	expectedRoutes := expectedManagedRoutes(settings, projectList, s.cfg.HTTPAddr)
	diagnostics.ManagedRouteCount = len(expectedRoutes)

	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rawConfig, err := s.caddy.GetConfig(checkCtx)
	if err != nil {
		diagnostics.Summary = "The live shared Caddy config could not be loaded."
		diagnostics.Detail = err.Error()
		return diagnostics
	}
	diagnostics.Available = true

	if pretty, err := prettyCaddyConfig(rawConfig); err != nil {
		diagnostics.RawConfigError = err.Error()
	} else if pretty != "" {
		diagnostics.RawConfig = pretty
		diagnostics.RawConfigAvailable = true
	}

	liveRoutes, err := caddyadmin.ExtractHTTPRoutes(rawConfig)
	if err != nil {
		diagnostics.Summary = "The live shared Caddy config was loaded, but its HTTP routes could not be parsed."
		diagnostics.Detail = err.Error()
		return diagnostics
	}
	diagnostics.LiveRouteCount = len(liveRoutes)

	liveByKey := map[string]caddyadmin.HTTPRoute{}
	for _, route := range liveRoutes {
		key := caddyadmin.RouteKey(route)
		if key == "||" || key == "" {
			continue
		}
		route.Upstreams = sortedValues(route.Upstreams)
		liveByKey[key] = route
	}

	for _, route := range expectedRoutes {
		expectedUpstreams := sortedValues(route.Upstreams)
		item := CaddyRouteDiagnostic{
			Host:              route.Host,
			MatcherSummary:    caddyadmin.MatcherSummary(route),
			ExpectedUpstreams: expectedUpstreams,
			Status:            "warning",
		}
		liveRoute, ok := liveByKey[caddyadmin.RouteKey(route)]
		if !ok {
			item.Detail = "Missing from the live shared Caddy config."
			diagnostics.DriftCount++
			diagnostics.Routes = append(diagnostics.Routes, item)
			continue
		}

		item.LiveUpstreams = liveRoute.Upstreams
		if !sameStrings(expectedUpstreams, liveRoute.Upstreams) {
			item.Detail = "The live upstream target differs from what CaddyTower expects."
			diagnostics.DriftCount++
			diagnostics.Routes = append(diagnostics.Routes, item)
			continue
		}

		item.Status = "ok"
		item.Detail = "Live route matches the expected upstream."
		diagnostics.HealthyRouteCount++
		diagnostics.Routes = append(diagnostics.Routes, item)
	}

	switch {
	case diagnostics.ManagedRouteCount == 0:
		diagnostics.Status = "warning"
		diagnostics.Summary = "No CaddyTower-managed routes are expected yet."
		diagnostics.Detail = "Finish the root-domain setup or add a web project to populate managed shared-Caddy routes."
	case diagnostics.DriftCount > 0:
		diagnostics.Status = "warning"
		diagnostics.Summary = fmt.Sprintf("%d of %d managed route%s need attention.", diagnostics.DriftCount, diagnostics.ManagedRouteCount, plural(diagnostics.ManagedRouteCount))
		diagnostics.Detail = "Expected routes are listed below so you can compare them with the live shared Caddy config."
	default:
		diagnostics.Status = "ok"
		diagnostics.Summary = fmt.Sprintf("Live shared Caddy config matches all %d managed route%s.", diagnostics.ManagedRouteCount, plural(diagnostics.ManagedRouteCount))
		diagnostics.Detail = "Use the raw config viewer below only for troubleshooting."
	}

	return diagnostics
}

func expectedManagedRoutes(settings Settings, projectList []Project, httpAddr string) []caddyadmin.HTTPRoute {
	routes := make([]caddyadmin.HTTPRoute, 0, len(projectList)+1)
	if adminHost := adminHostname(settings.RootDomain); adminHost != "" {
		routes = append(routes, caddyadmin.HTTPRoute{
			Host:       adminHost,
			MatchType:  httpRouteMatchHost,
			Upstreams:  []string{controllerContainerName + ":" + controllerPort(httpAddr)},
			Priority:   -1,
		})
	}

	for _, project := range projectList {
		if project.Type != projectTypeWeb {
			continue
		}
		routes = append(routes, expandedProjectRoutes(project)...)
	}

	sort.Slice(routes, func(i, j int) bool { return caddyadmin.RouteKey(routes[i]) < caddyadmin.RouteKey(routes[j]) })
	return routes
}

func expandedProjectRoutes(project Project) []caddyadmin.HTTPRoute {
	if project.Type != projectTypeWeb {
		return nil
	}

	hosts := effectiveProjectRouteHosts(project)
	if len(hosts) == 0 {
		return nil
	}
	upstream := project.ContainerName + ":" + strconv.Itoa(project.InternalPort)
	projectRoutes := project.HTTPRoutes
	if len(projectRoutes) == 0 {
		projectRoutes = []ProjectHTTPRoute{{MatchType: httpRouteMatchHost}}
	}

	routes := make([]caddyadmin.HTTPRoute, 0, len(projectRoutes)*len(hosts))
	for _, route := range projectRoutes {
		targetHosts := hosts
		if strings.TrimSpace(route.Hostname) != "" {
			targetHosts = []string{normalizeHostname(route.Hostname)}
		}
		for _, host := range targetHosts {
			if strings.TrimSpace(host) == "" {
				continue
			}
			routes = append(routes, caddyadmin.HTTPRoute{
				Host:          host,
				MatchType:     normalizeHTTPRouteMatchType(route.MatchType),
				MatchValue:    strings.TrimSpace(route.MatchValue),
				StripPrefix:   route.StripPrefix,
				RewritePrefix: strings.TrimSpace(route.RewritePrefix),
				Priority:      route.Priority,
				Upstreams:     []string{upstream},
			})
		}
	}
	sort.Slice(routes, func(i, j int) bool { return caddyadmin.RouteKey(routes[i]) < caddyadmin.RouteKey(routes[j]) })
	return routes
}

func effectiveProjectRouteHosts(project Project) []string {
	seen := map[string]struct{}{}
	hosts := make([]string, 0, len(project.CustomDomains)+1)
	add := func(host string) {
		host = normalizeHostname(host)
		if host == "" {
			return
		}
		if _, ok := seen[host]; ok {
			return
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	add(project.FullDomain)
	for _, domain := range project.CustomDomains {
		add(domain.Hostname)
	}
	sort.Strings(hosts)
	return hosts
}

func prettyCaddyConfig(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}

	var formatted bytes.Buffer
	if err := json.Indent(&formatted, raw, "", "  "); err != nil {
		return "", fmt.Errorf("format live Caddy config: %w", err)
	}
	return formatted.String(), nil
}

func sortedValues(values []string) []string {
	items := append([]string(nil), values...)
	sort.Strings(items)
	return items
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func envContainsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func dockerRequirementDetail(err error) string {
	message := err.Error()
	if strings.Contains(message, "permission denied while trying to connect to the Docker daemon socket") || strings.Contains(message, "dial unix /var/run/docker.sock: connect: permission denied") {
		return "The controller can see /var/run/docker.sock, but its container user is missing the host socket group. Recreate the caddytower container with a group_add entry that matches the Docker socket GID on the host."
	}
	return message
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

func (s *Service) SaveGitHubSettings(ctx context.Context, input GitHubSettingsInput, userID string) error {
	if s.store == nil {
		return fmt.Errorf("settings store unavailable")
	}
	if s.secrets == nil {
		return fmt.Errorf("CADDYTOWER_MASTER_KEY is required to store GitHub App credentials securely")
	}
	if input.AppID <= 0 {
		return fmt.Errorf("github app id must be greater than zero")
	}
	appSlug := strings.TrimSpace(input.AppSlug)
	if appSlug == "" {
		return fmt.Errorf("github app slug is required")
	}

	current, err := s.GitHubSettings(ctx)
	if err != nil {
		return err
	}
	webhookSecret := strings.TrimSpace(input.WebhookSecret)
	if webhookSecret == "" {
		webhookSecret = current.WebhookSecret
	}
	if webhookSecret == "" {
		return fmt.Errorf("github webhook secret is required")
	}

	privateKeyPEM := strings.TrimSpace(input.PrivateKeyPEM)
	if privateKeyPEM == "" {
		privateKeyPEM = current.PrivateKeyPEM
	}
	if privateKeyPEM == "" && !current.PrivateKeyPresent {
		return fmt.Errorf("github app private key pem is required")
	}
	if privateKeyPEM != "" {
		if err := githubapp.ValidatePrivateKeyPEM(privateKeyPEM); err != nil {
			return fmt.Errorf("validate github app private key: %w", err)
		}
	}

	values, err := s.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	values[settingGitHubAppID] = strconv.FormatInt(input.AppID, 10)
	values[settingGitHubAppSlug] = appSlug

	encodedSecret, err := s.encodeSecret(webhookSecret)
	if err != nil {
		return err
	}
	values[settingGitHubWebhook] = encodedSecret

	if privateKeyPEM != "" {
		encodedKey, err := s.encodeSecret(privateKeyPEM)
		if err != nil {
			return err
		}
		values[settingGitHubPrivateKey] = encodedKey
	}

	if err := s.store.UpsertSettings(ctx, values); err != nil {
		return err
	}
	if userID != "" {
		if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "settings.update", "settings:github", map[string]any{
			"app_id":         input.AppID,
			"app_slug":       appSlug,
			"webhook_secret": true,
			"private_key":    privateKeyPEM != "" || current.PrivateKeyPresent,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) GitHubSettings(ctx context.Context) (GitHubSettings, error) {
	if s.store == nil {
		return GitHubSettings{
			AppID:                s.cfg.GitHubAppID,
			AppSlug:              strings.TrimSpace(s.cfg.GitHubAppSlug),
			WebhookSecret:        strings.TrimSpace(s.cfg.GitHubWebhookSecret),
			PrivateKeyPath:       strings.TrimSpace(s.cfg.GitHubAppPrivateKeyPath),
			WebhookSecretPresent: strings.TrimSpace(s.cfg.GitHubWebhookSecret) != "",
			PrivateKeyPresent:    strings.TrimSpace(s.cfg.GitHubAppPrivateKeyPath) != "",
			Configured:           s.cfg.GitHubConfigured(),
		}, nil
	}

	raw, err := s.store.GetSettings(ctx, settingGitHubAppID, settingGitHubAppSlug, settingGitHubWebhook, settingGitHubPrivateKey)
	if err != nil {
		return GitHubSettings{}, err
	}

	settings := GitHubSettings{
		AppID:                s.cfg.GitHubAppID,
		AppSlug:              strings.TrimSpace(s.cfg.GitHubAppSlug),
		WebhookSecret:        strings.TrimSpace(s.cfg.GitHubWebhookSecret),
		PrivateKeyPath:       strings.TrimSpace(s.cfg.GitHubAppPrivateKeyPath),
		WebhookSecretPresent: strings.TrimSpace(s.cfg.GitHubWebhookSecret) != "",
		PrivateKeyPresent:    strings.TrimSpace(s.cfg.GitHubAppPrivateKeyPath) != "",
	}

	if value := strings.TrimSpace(raw[settingGitHubAppID]); value != "" {
		settings.StoredInApp = true
		appID, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return GitHubSettings{}, fmt.Errorf("parse stored github app id: %w", err)
		}
		settings.AppID = appID
	}
	if value := strings.TrimSpace(raw[settingGitHubAppSlug]); value != "" {
		settings.StoredInApp = true
		settings.AppSlug = value
	}
	if value := strings.TrimSpace(raw[settingGitHubWebhook]); value != "" {
		settings.StoredInApp = true
		decoded, err := s.decodeSecret(value)
		if err != nil {
			return GitHubSettings{}, err
		}
		settings.WebhookSecret = decoded
		settings.WebhookSecretPresent = decoded != ""
	}
	if value := strings.TrimSpace(raw[settingGitHubPrivateKey]); value != "" {
		settings.StoredInApp = true
		decoded, err := s.decodeSecret(value)
		if err != nil {
			return GitHubSettings{}, err
		}
		settings.PrivateKeyPEM = decoded
		settings.PrivateKeyPath = ""
		settings.PrivateKeyPresent = decoded != ""
	}
	settings.Configured = settings.AppID > 0 && settings.AppSlug != "" && settings.WebhookSecret != "" && settings.PrivateKeyPresent
	return settings, nil
}

func (s *Service) GitHubService(ctx context.Context) (*githubapp.Service, error) {
	settings, err := s.GitHubSettings(ctx)
	if err != nil {
		return nil, err
	}
	return githubapp.New(githubapp.Config{
		AppID:          settings.AppID,
		AppSlug:        settings.AppSlug,
		PrivateKeyPEM:  settings.PrivateKeyPEM,
		PrivateKeyPath: settings.PrivateKeyPath,
		WebhookSecret:  settings.WebhookSecret,
		APIBaseURL:     s.cfg.GitHubAPIBaseURL,
		WebBaseURL:     s.cfg.GitHubWebBaseURL,
	}, s.store, nil), nil
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
	parsedMounts, err := parseMountsText(input.MountsText)
	if err != nil {
		return store.ProjectRecord{}, err
	}
	mounts := projectMountsToStore(parsedMounts)
	var httpRoutes []store.ProjectHTTPRouteRecord
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
		parsedRoutes, err := parseHTTPRoutesText(input.HTTPRoutesText)
		if err != nil {
			return store.ProjectRecord{}, err
		}
		httpRoutes = projectHTTPRoutesToStore(parsedRoutes)
	case projectTypeTCP, projectTypeUDP:
		if strings.TrimSpace(input.HTTPRoutesText) != "" {
			return store.ProjectRecord{}, fmt.Errorf("HTTP route rules are only available for web projects")
		}
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
		Mounts:                    mounts,
		Ports:                     ports,
		HTTPRoutes:                httpRoutes,
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

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	return s.loadSettings(ctx)
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
	projectHTTPRoutes := []ProjectHTTPRoute(nil)
	projectHTTPRoutesText := ""
	if record.Type == projectTypeWeb {
		projectHTTPRoutes = defaultProjectHTTPRoutes(record.HTTPRoutes)
		projectHTTPRoutesText = httpRoutesText(projectHTTPRoutes)
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
		Mounts:                    projectMountsFromStore(record.Mounts),
		WatchtowerEnabled:         record.WatchtowerEnabled,
		Env:                       env,
		EnvText:                   envText(env),
		MountsText:                mountsText(projectMountsFromStore(record.Mounts)),
		HTTPRoutes:                projectHTTPRoutes,
		HTTPRoutesText:            projectHTTPRoutesText,
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
			Mounts:        dockerMounts(project.Mounts),
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
	managedRouteKeys := make([]string, 0, len(records))
	if settings.RootDomain != "" {
		adminHost := adminHostname(settings.RootDomain)
		routes = append(routes, caddyadmin.HTTPRoute{
			Host:      adminHost,
			MatchType: httpRouteMatchHost,
			Upstreams: []string{controllerContainerName + ":" + controllerPort(s.cfg.HTTPAddr)},
			Priority:  -1,
		})
		managedRouteKeys = append(managedRouteKeys, caddyadmin.RouteKey(routes[len(routes)-1]))
	}
	for _, record := range records {
		if record.Type != projectTypeWeb {
			continue
		}
		project, err := s.projectFromRecord(ctx, record, settings)
		if err != nil {
			return err
		}
		projectRoutes := expandedProjectRoutes(project)
		routes = append(routes, projectRoutes...)
		for _, route := range projectRoutes {
			managedRouteKeys = append(managedRouteKeys, caddyadmin.RouteKey(route))
		}
	}
	if len(managedRouteKeys) == 0 {
		return nil
	}
	if settings.RootDomain == "" {
		return fmt.Errorf("root domain is required for caddy reconciliation")
	}

	_, err = s.caddy.ReconcileManagedRoutes(ctx, routes, managedRouteKeys)
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

func projectMountsFromStore(records []store.ProjectMountRecord) []ProjectMount {
	if len(records) == 0 {
		return nil
	}

	mounts := make([]ProjectMount, 0, len(records))
	for _, record := range records {
		mounts = append(mounts, ProjectMount{
			Type:     record.Type,
			Source:   record.Source,
			Target:   record.Target,
			ReadOnly: record.ReadOnly,
		})
	}
	return mounts
}

func projectHTTPRoutesFromStore(records []store.ProjectHTTPRouteRecord) []ProjectHTTPRoute {
	if len(records) == 0 {
		return nil
	}

	routes := make([]ProjectHTTPRoute, 0, len(records))
	for _, record := range records {
		routes = append(routes, ProjectHTTPRoute{
			Hostname:      record.Hostname,
			MatchType:     record.MatchType,
			MatchValue:    record.MatchValue,
			StripPrefix:   record.StripPrefix,
			RewritePrefix: record.RewritePrefix,
			Priority:      record.Priority,
		})
	}
	return routes
}

func defaultProjectHTTPRoutes(records []store.ProjectHTTPRouteRecord) []ProjectHTTPRoute {
	routes := projectHTTPRoutesFromStore(records)
	if len(routes) > 0 {
		return routes
	}
	return []ProjectHTTPRoute{{
		MatchType: httpRouteMatchHost,
		Priority:  0,
	}}
}

func dockerMounts(mounts []ProjectMount) []dockerx.Mount {
	if len(mounts) == 0 {
		return nil
	}

	result := make([]dockerx.Mount, 0, len(mounts))
	for _, mount := range mounts {
		result = append(result, dockerx.Mount{
			Source:   mount.Source,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
		})
	}
	return result
}

func projectMountsToStore(mounts []ProjectMount) []store.ProjectMountRecord {
	if len(mounts) == 0 {
		return nil
	}
	result := make([]store.ProjectMountRecord, 0, len(mounts))
	for _, mount := range mounts {
		result = append(result, store.ProjectMountRecord{
			Type:     mount.Type,
			Source:   mount.Source,
			Target:   mount.Target,
			ReadOnly: mount.ReadOnly,
		})
	}
	return result
}

func projectHTTPRoutesToStore(routes []ProjectHTTPRoute) []store.ProjectHTTPRouteRecord {
	if len(routes) == 0 {
		return nil
	}
	result := make([]store.ProjectHTTPRouteRecord, 0, len(routes))
	for _, route := range routes {
		result = append(result, store.ProjectHTTPRouteRecord{
			Hostname:      route.Hostname,
			MatchType:     route.MatchType,
			MatchValue:    route.MatchValue,
			StripPrefix:   route.StripPrefix,
			RewritePrefix: route.RewritePrefix,
			Priority:      route.Priority,
		})
	}
	return result
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

func mountsText(mounts []ProjectMount) string {
	if len(mounts) == 0 {
		return ""
	}

	lines := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		mode := "rw"
		if mount.ReadOnly {
			mode = "ro"
		}
		lines = append(lines, fmt.Sprintf("%s | %s | %s", mount.Source, mount.Target, mode))
	}
	return strings.Join(lines, "\n")
}

func parseMountsText(raw string) ([]ProjectMount, error) {
	lines := strings.Split(raw, "\n")
	mounts := make([]ProjectMount, 0, len(lines))
	seenTargets := map[string]struct{}{}
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := splitDelimitedLine(line, "|")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("invalid mount on line %d: use source | target | ro", index+1)
		}
		source := strings.TrimSpace(parts[0])
		target := strings.TrimSpace(parts[1])
		mode := "rw"
		if len(parts) == 3 {
			mode = strings.TrimSpace(strings.ToLower(parts[2]))
		}
		if !strings.HasPrefix(source, "/") {
			return nil, fmt.Errorf("mount source on line %d must be an absolute host path", index+1)
		}
		if !strings.HasPrefix(target, "/") {
			return nil, fmt.Errorf("mount target on line %d must be an absolute container path", index+1)
		}
		if isReservedMountTarget(target) {
			return nil, fmt.Errorf("mount target %s on line %d is reserved", target, index+1)
		}
		if _, ok := seenTargets[target]; ok {
			return nil, fmt.Errorf("mount target %s is listed more than once", target)
		}
		seenTargets[target] = struct{}{}

		readOnly := false
		switch mode {
		case "", "rw", "readwrite":
		case "ro", "readonly":
			readOnly = true
		default:
			return nil, fmt.Errorf("mount mode on line %d must be rw or ro", index+1)
		}

		mounts = append(mounts, ProjectMount{
			Type:     mountTypeBind,
			Source:   source,
			Target:   target,
			ReadOnly: readOnly,
		})
	}
	return mounts, nil
}

func httpRoutesText(routes []ProjectHTTPRoute) string {
	if len(routes) == 0 {
		return ""
	}

	lines := make([]string, 0, len(routes))
	for _, route := range routes {
		host := route.Hostname
		if strings.TrimSpace(host) == "" {
			host = httpRouteAllDomainsScope
		}
		line := []string{host, route.MatchType}
		if route.MatchType != httpRouteMatchHost {
			line = append(line, route.MatchValue)
		}
		transform := ""
		if route.StripPrefix {
			transform = "strip"
		} else if strings.TrimSpace(route.RewritePrefix) != "" {
			transform = "rewrite=" + route.RewritePrefix
		}
		if route.MatchType != httpRouteMatchHost || transform != "" {
			line = append(line, transform)
		}
		lines = append(lines, strings.Join(line, " | "))
	}
	return strings.Join(lines, "\n")
}

func parseHTTPRoutesText(raw string) ([]ProjectHTTPRoute, error) {
	lines := strings.Split(raw, "\n")
	routes := make([]ProjectHTTPRoute, 0, len(lines))
	seen := map[string]struct{}{}
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := splitDelimitedLine(line, "|")
		if len(parts) < 2 || len(parts) > 4 {
			return nil, fmt.Errorf("invalid HTTP route on line %d: use host | match_type | match_value | strip or rewrite=/prefix", index+1)
		}
		host := strings.TrimSpace(parts[0])
		matchType := normalizeHTTPRouteMatchType(parts[1])
		matchValue := ""
		if len(parts) >= 3 {
			matchValue = strings.TrimSpace(parts[2])
		}
		transform := ""
		if len(parts) == 4 {
			transform = strings.TrimSpace(parts[3])
		}
		if host == httpRouteAllDomainsScope {
			host = ""
		}
		if host != "" {
			host = normalizeHostname(host)
			if host == "" || strings.Contains(host, " ") {
				return nil, fmt.Errorf("HTTP route host on line %d is invalid", index+1)
			}
		}
		route := ProjectHTTPRoute{
			Hostname:  host,
			MatchType: matchType,
			Priority:  len(routes),
		}
		switch matchType {
		case httpRouteMatchHost:
			if matchValue != "" {
				return nil, fmt.Errorf("host routes on line %d must not set a match value", index+1)
			}
		case httpRouteMatchPathPrefix, httpRouteMatchPathExact:
			if !strings.HasPrefix(matchValue, "/") {
				return nil, fmt.Errorf("HTTP route match value on line %d must start with /", index+1)
			}
			route.MatchValue = matchValue
		default:
			return nil, fmt.Errorf("HTTP route type on line %d must be host, path_prefix, or path_exact", index+1)
		}

		switch {
		case transform == "":
		case strings.EqualFold(transform, "strip"):
			if matchType != httpRouteMatchPathPrefix {
				return nil, fmt.Errorf("strip is only valid for path_prefix routes on line %d", index+1)
			}
			route.StripPrefix = true
		case strings.HasPrefix(strings.ToLower(transform), "rewrite="):
			if matchType == httpRouteMatchHost {
				return nil, fmt.Errorf("rewrite is only valid for path routes on line %d", index+1)
			}
			rewritePrefix := strings.TrimSpace(transform[len("rewrite="):])
			if !strings.HasPrefix(rewritePrefix, "/") {
				return nil, fmt.Errorf("rewrite target on line %d must start with /", index+1)
			}
			route.RewritePrefix = rewritePrefix
		default:
			return nil, fmt.Errorf("unknown HTTP route transform on line %d", index+1)
		}

		key := strings.Join([]string{route.Hostname, route.MatchType, route.MatchValue}, "|")
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("HTTP route on line %d duplicates an earlier route", index+1)
		}
		seen[key] = struct{}{}
		routes = append(routes, route)
	}
	return routes, nil
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

func normalizeHTTPRouteMatchType(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case httpRouteMatchPathPrefix, "prefix":
		return httpRouteMatchPathPrefix
	case httpRouteMatchPathExact, "exact":
		return httpRouteMatchPathExact
	default:
		return httpRouteMatchHost
	}
}

func splitDelimitedLine(value, delimiter string) []string {
	rawParts := strings.Split(value, delimiter)
	parts := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		parts = append(parts, strings.TrimSpace(part))
	}
	return parts
}

func isReservedMountTarget(target string) bool {
	switch strings.TrimSpace(target) {
	case "/var/run/docker.sock", "/data":
		return true
	default:
		return false
	}
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
