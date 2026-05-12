package projects

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
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
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

type dockerService interface {
	PullImage(context.Context, string) error
	RecreateContainer(context.Context, dockerx.ContainerSpec) (dockerx.ContainerInspect, error)
	InspectContainer(context.Context, string) (dockerx.ContainerInspect, error)
	ListContainersByLabel(context.Context, string, string) ([]dockerx.ContainerSummary, error)
	RemoveContainer(context.Context, string) error
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
	UpsertCNAME(context.Context, string, string, string, bool) (cloudflare.DNSRecord, bool, error)
	DeleteCNAME(context.Context, string, string) error
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
	ID                string
	Name              string
	Slug              string
	WebhookSecret     string
	ImageRef          string
	Subdomain         string
	FullDomain        string
	ContainerName     string
	InternalPort      int
	WatchtowerEnabled bool
	Env               map[string]string
	EnvText           string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Status            string
	DBAttachments     []DBAttachment
}

type Dashboard struct {
	Settings Settings
	Projects []Project
}

type WebProjectInput struct {
	ID                string
	Name              string
	Slug              string
	ImageRef          string
	Subdomain         string
	InternalPort      int
	WatchtowerEnabled bool
	EnvText           string
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
	return &Service{
		cfg:     cfg,
		store:   stateStore,
		secrets: secretService,
		docker:  dockerSvc,
		caddy:   caddySvc,
		db:      dbengines.New(stateStore, secretService, dockerSvc),
		logger:  logger,
		newCloudflare: func(token string) (cloudflareClient, error) {
			return cloudflare.New(token)
		},
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
	values, err := s.store.GetSettings(ctx)
	if err != nil {
		return err
	}

	rootDomain := strings.TrimSpace(strings.ToLower(input.RootDomain))
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

	return s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "settings.update", "settings:deployment", map[string]any{
		"root_domain":        rootDomain,
		"origin_hostname":    originHostname,
		"cloudflare_zone_id": zoneID,
		"cloudflare_token":   tokenValue != "",
		"cloudflare_proxied": input.CloudflareProxied,
	})
}

func (s *Service) CreateWebProject(ctx context.Context, input WebProjectInput, userID string) (Project, error) {
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

	if err := s.applyProject(ctx, project, settings); err != nil {
		return Project{}, fmt.Errorf("project saved but deployment failed: %w", err)
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.create", "project:"+record.ID, map[string]any{
		"slug":      record.Slug,
		"subdomain": record.Subdomain,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) UpdateWebProject(ctx context.Context, input WebProjectInput, userID string) (Project, error) {
	current, err := s.store.GetProject(ctx, input.ID)
	if err != nil {
		return Project{}, err
	}

	record, err := s.recordFromInput(input, current.Slug)
	if err != nil {
		return Project{}, err
	}
	record.ID = current.ID
	record.Slug = current.Slug
	record.Type = current.Type
	record.WebhookSecret = current.WebhookSecret

	if err := s.store.UpdateProject(ctx, record); err != nil {
		return Project{}, err
	}

	project, settings, err := s.GetProject(ctx, record.ID)
	if err != nil {
		return Project{}, err
	}

	if err := s.applyProject(ctx, project, settings); err != nil {
		return Project{}, fmt.Errorf("project updated but deployment failed: %w", err)
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

	if err := s.applyProject(ctx, project, settings); err != nil {
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

	if err := s.applyProject(ctx, project, settings); err != nil {
		return Project{}, err
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), "", "project.webhook_redeploy", "project:"+project.ID, map[string]any{
		"slug": project.Slug,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
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
	if err := s.applyProject(ctx, project, settings); err != nil {
		return Project{}, fmt.Errorf("database attached but project redeploy failed: %w", err)
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
	if err := s.applyProject(ctx, project, settings); err != nil {
		return Project{}, fmt.Errorf("credentials rotated but project redeploy failed: %w", err)
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
	if err := s.applyProject(ctx, project, settings); err != nil {
		return Project{}, fmt.Errorf("database detached but project redeploy failed: %w", err)
	}

	if err := s.store.InsertAuditLog(ctx, uuid.NewString(), userID, "project.db.delete", "project:"+projectID, map[string]any{
		"attachment": attachmentID,
	}); err != nil {
		return Project{}, err
	}

	return project, nil
}

func (s *Service) recordFromInput(input WebProjectInput, existingSlug string) (store.ProjectRecord, error) {
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
	if subdomain == "" {
		return store.ProjectRecord{}, fmt.Errorf("subdomain is required")
	}
	if strings.Contains(subdomain, " ") || strings.HasPrefix(subdomain, ".") || strings.HasSuffix(subdomain, ".") {
		return store.ProjectRecord{}, fmt.Errorf("subdomain is invalid")
	}
	if input.InternalPort <= 0 || input.InternalPort > 65535 {
		return store.ProjectRecord{}, fmt.Errorf("internal port must be between 1 and 65535")
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
		ID:                id,
		Slug:              slug,
		Name:              name,
		Type:              "web",
		ImageRef:          imageRef,
		InternalPort:      input.InternalPort,
		Subdomain:         subdomain,
		WatchtowerEnabled: input.WatchtowerEnabled,
		WebhookSecret:     randomSecret(),
		Env:               env,
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

	return Project{
		ID:                record.ID,
		Name:              record.Name,
		Slug:              record.Slug,
		WebhookSecret:     record.WebhookSecret,
		ImageRef:          record.ImageRef,
		Subdomain:         record.Subdomain,
		FullDomain:        fqdn(settings.RootDomain, record.Subdomain),
		ContainerName:     containerName(record.Slug),
		InternalPort:      record.InternalPort,
		WatchtowerEnabled: record.WatchtowerEnabled,
		Env:               env,
		EnvText:           envText(env),
		CreatedAt:         record.CreatedAt,
		UpdatedAt:         record.UpdatedAt,
		Status:            status,
		DBAttachments:     attachments,
	}, nil
}

func (s *Service) applyProject(ctx context.Context, project Project, settings Settings) error {
	if settings.RootDomain == "" {
		return fmt.Errorf("configure the root domain before deploying projects")
	}

	if s.docker != nil {
		if err := s.docker.PullImage(ctx, project.ImageRef); err != nil {
			return err
		}

		labels := map[string]string{
			"caddytower.managed": "true",
			"caddytower.project": project.Slug,
		}
		if project.WatchtowerEnabled {
			labels["com.centurylinklabs.watchtower.enable"] = "true"
		}

		if _, err := s.docker.RecreateContainer(ctx, dockerx.ContainerSpec{
			Name:          project.ContainerName,
			Image:         project.ImageRef,
			Env:           project.runtimeEnv(),
			Labels:        labels,
			Network:       managedNetworkName,
			ExposedPorts:  []string{strconv.Itoa(project.InternalPort)},
			RestartPolicy: "unless-stopped",
		}); err != nil {
			return err
		}
	}

	if err := s.reconcileCaddy(ctx); err != nil {
		return err
	}

	if err := s.upsertCloudflare(ctx, project, settings); err != nil {
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
	if settings.RootDomain == "" {
		return fmt.Errorf("root domain is required for caddy reconciliation")
	}

	records, err := s.store.ListProjects(ctx)
	if err != nil {
		return err
	}

	routes := make([]caddyadmin.HTTPRoute, 0, len(records))
	managedHosts := make([]string, 0, len(records))
	for _, record := range records {
		if record.Type != "web" {
			continue
		}
		host := fqdn(settings.RootDomain, record.Subdomain)
		routes = append(routes, caddyadmin.HTTPRoute{
			Host:      host,
			Upstreams: []string{containerName(record.Slug) + ":" + strconv.Itoa(record.InternalPort)},
		})
		managedHosts = append(managedHosts, host)
	}

	_, err = s.caddy.ReconcileManagedRoutes(ctx, routes, managedHosts)
	return err
}

func (s *Service) upsertCloudflare(ctx context.Context, project Project, settings Settings) error {
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

	_, _, err = client.UpsertCNAME(ctx, settings.CloudflareZoneID, project.FullDomain, settings.OriginHostname, settings.CloudflareProxied)
	return err
}

func (s *Service) deleteCloudflare(ctx context.Context, project Project, settings Settings) error {
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

	return client.DeleteCNAME(ctx, settings.CloudflareZoneID, project.FullDomain)
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
	if rootDomain == "" {
		return subdomain
	}
	return strings.TrimSuffix(subdomain, ".") + "." + strings.TrimPrefix(rootDomain, ".")
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
