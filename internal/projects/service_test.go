package projects

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"caddytower/internal/caddyadmin"
	"caddytower/internal/cloudflare"
	"caddytower/internal/config"
	"caddytower/internal/dbengines"
	"caddytower/internal/dockerx"
	"caddytower/internal/secrets"
	"caddytower/internal/store"

	"github.com/google/uuid"
)

func TestCreateWebProjectDeploysAndPersists(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	secretSvc, err := secrets.NewOptionalFromBase64("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	if err != nil {
		t.Fatalf("NewOptionalFromBase64() error = %v", err)
	}

	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	cloudflareFactory := &fakeCloudflareFactory{}

	svc := New(config.Config{RootDomain: "example.com"}, stateStore, secretSvc, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.newCloudflare = cloudflareFactory.New

	if err := svc.SaveSettings(context.Background(), SettingsInput{
		RootDomain:        "example.com",
		OriginHostname:    "origin.example.com",
		CloudflareZoneID:  "zone-1",
		CloudflareToken:   "token-1",
		CloudflareProxied: true,
	}, ""); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	project, err := svc.CreateWebProject(context.Background(), WebProjectInput{
		Name:              "Demo",
		Slug:              "demo",
		ImageRef:          "ghcr.io/example/demo:latest",
		Subdomain:         "demo",
		InternalPort:      3000,
		WatchtowerEnabled: true,
		EnvText:           "NODE_ENV=production\nAPI_KEY=secret",
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	if project.FullDomain != "demo.example.com" {
		t.Fatalf("project.FullDomain = %q", project.FullDomain)
	}
	if docker.recreateCount != 1 {
		t.Fatalf("recreateCount = %d", docker.recreateCount)
	}
	if len(caddy.managedHosts) != 1 || caddy.managedHosts[0] != "demo.example.com" {
		t.Fatalf("managed hosts = %#v", caddy.managedHosts)
	}
	if cloudflareFactory.client.upsertCount != 1 {
		t.Fatalf("upsertCount = %d", cloudflareFactory.client.upsertCount)
	}

	stored, _, err := svc.GetProject(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("GetProject() error = %v", err)
	}
	if stored.Env["API_KEY"] != "secret" {
		t.Fatalf("stored env = %#v", stored.Env)
	}
}

func TestDeleteProjectRemovesManagedResources(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	cloudflareFactory := &fakeCloudflareFactory{}
	svc := New(config.Config{RootDomain: "example.com"}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.newCloudflare = cloudflareFactory.New

	if err := svc.SaveSettings(context.Background(), SettingsInput{
		RootDomain:       "example.com",
		OriginHostname:   "origin.example.com",
		CloudflareZoneID: "zone-1",
		CloudflareToken:  "token-1",
	}, ""); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	project, err := svc.CreateWebProject(context.Background(), WebProjectInput{
		Name:         "Demo",
		Slug:         "demo",
		ImageRef:     "ghcr.io/example/demo:latest",
		Subdomain:    "demo",
		InternalPort: 3000,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	if err := svc.DeleteProject(context.Background(), project.ID, ""); err != nil {
		t.Fatalf("DeleteProject() error = %v", err)
	}

	if docker.removeCount != 1 {
		t.Fatalf("removeCount = %d", docker.removeCount)
	}
	if cloudflareFactory.client.deleteCount != 1 {
		t.Fatalf("deleteCount = %d", cloudflareFactory.client.deleteCount)
	}
}

func TestCreateTCPProjectPublishesPortsWithoutCaddy(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	svc := New(config.Config{}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))

	project, err := svc.CreateProject(context.Background(), WebProjectInput{
		Type:              "tcp",
		Name:              "Netserver",
		Slug:              "netserver",
		ImageRef:          "ghcr.io/example/netserver:latest",
		PortMappingsText:  "25565:25565\n25566:25566",
		WatchtowerEnabled: true,
	}, "")
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}

	if project.Type != "tcp" {
		t.Fatalf("project.Type = %q", project.Type)
	}
	if len(project.Ports) != 2 {
		t.Fatalf("ports = %#v", project.Ports)
	}
	if len(docker.lastSpec.PublishedPorts) != 2 {
		t.Fatalf("published ports = %#v", docker.lastSpec.PublishedPorts)
	}
	if len(caddy.managedHosts) != 0 {
		t.Fatalf("managed hosts = %#v", caddy.managedHosts)
	}
}

func TestAttachDatabaseAddsAttachmentAndRuntimeEnv(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	secretSvc, err := secrets.NewOptionalFromBase64("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	if err != nil {
		t.Fatalf("NewOptionalFromBase64() error = %v", err)
	}

	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	svc := New(config.Config{RootDomain: "example.com"}, stateStore, secretSvc, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.db = &fakeDBService{}

	project, err := svc.CreateWebProject(context.Background(), WebProjectInput{
		Name:         "Demo",
		Slug:         "demo",
		ImageRef:     "ghcr.io/example/demo:latest",
		Subdomain:    "demo",
		InternalPort: 3000,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	project, err = svc.AttachDatabase(context.Background(), DatabaseAttachmentInput{
		ProjectID:  project.ID,
		Engine:     "pg",
		EnvVarName: "DATABASE_URL",
	}, "")
	if err != nil {
		t.Fatalf("AttachDatabase() error = %v", err)
	}

	if len(project.DBAttachments) != 1 {
		t.Fatalf("attachments = %#v", project.DBAttachments)
	}
	if project.DBAttachments[0].EnvVarName != "DATABASE_URL" || project.DBAttachments[0].ConnectionHint == "" {
		t.Fatalf("attachment = %#v", project.DBAttachments[0])
	}
	if docker.recreateCount != 2 {
		t.Fatalf("recreateCount = %d", docker.recreateCount)
	}
}

func TestDeleteProjectDropsAttachedDatabases(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	fakeDB := &fakeDBService{}
	svc := New(config.Config{RootDomain: "example.com"}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.db = fakeDB

	project, err := svc.CreateWebProject(context.Background(), WebProjectInput{
		Name:         "Demo",
		Slug:         "demo",
		ImageRef:     "ghcr.io/example/demo:latest",
		Subdomain:    "demo",
		InternalPort: 3000,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	if _, err := svc.AttachDatabase(context.Background(), DatabaseAttachmentInput{
		ProjectID: project.ID,
		Engine:    "pg",
	}, ""); err != nil {
		t.Fatalf("AttachDatabase() error = %v", err)
	}

	if err := svc.DeleteProject(context.Background(), project.ID, ""); err != nil {
		t.Fatalf("DeleteProject() error = %v", err)
	}

	if len(fakeDB.deleted) != 1 {
		t.Fatalf("deleted attachments = %#v", fakeDB.deleted)
	}
}

func TestRedeployProjectByWebhookUsesSlug(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	svc := New(config.Config{RootDomain: "example.com"}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))

	project, err := svc.CreateWebProject(context.Background(), WebProjectInput{
		Name:         "Demo",
		Slug:         "demo",
		ImageRef:     "ghcr.io/example/demo:latest",
		Subdomain:    "demo",
		InternalPort: 3000,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	redeployed, err := svc.RedeployProjectByWebhook(context.Background(), "demo")
	if err != nil {
		t.Fatalf("RedeployProjectByWebhook() error = %v", err)
	}

	if redeployed.ID != project.ID {
		t.Fatalf("redeployed.ID = %q, want %q", redeployed.ID, project.ID)
	}
	if docker.recreateCount != 2 {
		t.Fatalf("recreateCount = %d", docker.recreateCount)
	}
}

func TestStreamProjectLogsUsesContainerName(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{logContent: "hello\nworld\n"}
	caddy := &fakeCaddy{}
	svc := New(config.Config{RootDomain: "example.com"}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))

	project, err := svc.CreateWebProject(context.Background(), WebProjectInput{
		Name:         "Demo",
		Slug:         "demo",
		ImageRef:     "ghcr.io/example/demo:latest",
		Subdomain:    "demo",
		InternalPort: 3000,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	reader, err := svc.StreamProjectLogs(context.Background(), project.ID, 50)
	if err != nil {
		t.Fatalf("StreamProjectLogs() error = %v", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	if string(data) != "hello\nworld\n" {
		t.Fatalf("logs = %q", string(data))
	}
}

func TestAdoptExistingImportsWebContainer(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{
		containers: []dockerx.ContainerSummary{
			{Name: "cameos-editor", Image: "ghcr.io/example/cameos:latest"},
		},
		inspectByName: map[string]dockerx.ContainerInspect{
			"cameos-editor": {
				Name:  "cameos-editor",
				Image: "ghcr.io/example/cameos:latest",
				Env:   []string{"APP_ENV=prod"},
			},
		},
	}
	caddy := &fakeCaddy{
		config: json.RawMessage(`{
			"apps": {"http": {"servers": {"srv0": {"routes": [
				{"match":[{"host":["cameos.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"cameos-editor:8080"}]}],"terminal":true}
			]}}}}
		}`),
	}
	svc := New(config.Config{}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := svc.SaveSettings(context.Background(), SettingsInput{RootDomain: "example.com"}, ""); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	adopted, err := svc.AdoptExisting(context.Background(), "")
	if err != nil {
		t.Fatalf("AdoptExisting() error = %v", err)
	}

	if len(adopted) != 1 {
		t.Fatalf("adopted = %#v", adopted)
	}
	if adopted[0].Type != "web" || adopted[0].Subdomain != "cameos" || adopted[0].InternalPort != 8080 {
		t.Fatalf("project = %#v", adopted[0])
	}
}

func openProjectsStore(t *testing.T) *store.Store {
	t.Helper()

	stateStore, err := store.Open(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close() })
	return stateStore
}

type fakeDocker struct {
	recreateCount int
	removeCount   int
	lastSpec      dockerx.ContainerSpec
	logContent    string
	containers    []dockerx.ContainerSummary
	inspectByName map[string]dockerx.ContainerInspect
}

func (f *fakeDocker) PullImage(context.Context, string) error { return nil }
func (f *fakeDocker) RecreateContainer(_ context.Context, spec dockerx.ContainerSpec) (dockerx.ContainerInspect, error) {
	f.recreateCount++
	f.lastSpec = spec
	return dockerx.ContainerInspect{
		ID:      uuid.NewString(),
		Name:    spec.Name,
		Image:   spec.Image,
		Running: true,
	}, nil
}
func (f *fakeDocker) InspectContainer(_ context.Context, name string) (dockerx.ContainerInspect, error) {
	if inspect, ok := f.inspectByName[name]; ok {
		return inspect, nil
	}
	return dockerx.ContainerInspect{Running: true}, nil
}
func (f *fakeDocker) ListContainersByLabel(context.Context, string, string) ([]dockerx.ContainerSummary, error) {
	return nil, nil
}
func (f *fakeDocker) ListContainers(context.Context) ([]dockerx.ContainerSummary, error) {
	return append([]dockerx.ContainerSummary(nil), f.containers...), nil
}
func (f *fakeDocker) RemoveContainer(context.Context, string) error {
	f.removeCount++
	return nil
}
func (f *fakeDocker) StreamLogs(context.Context, string, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.logContent)), nil
}
func (f *fakeDocker) Exec(context.Context, string, []string, []string, io.Writer, io.Writer) error {
	return nil
}

type fakeCaddy struct {
	managedHosts []string
	config       json.RawMessage
}

func (f *fakeCaddy) GetConfig(context.Context) (json.RawMessage, error) {
	if len(f.config) != 0 {
		return f.config, nil
	}
	return json.RawMessage(`{}`), nil
}
func (f *fakeCaddy) ReconcileManagedRoutes(_ context.Context, routes []caddyadmin.HTTPRoute, managedHosts []string) (bool, error) {
	f.managedHosts = append([]string(nil), managedHosts...)
	return true, nil
}

type fakeCloudflareFactory struct {
	client *fakeCloudflare
}

func (f *fakeCloudflareFactory) New(string) (cloudflareClient, error) {
	if f.client == nil {
		f.client = &fakeCloudflare{}
	}
	return f.client, nil
}

type fakeCloudflare struct {
	upsertCount int
	deleteCount int
}

func (f *fakeCloudflare) ValidateToken(context.Context) error { return nil }
func (f *fakeCloudflare) UpsertCNAME(context.Context, string, string, string, bool) (cloudflare.DNSRecord, bool, error) {
	f.upsertCount++
	return cloudflare.DNSRecord{}, true, nil
}
func (f *fakeCloudflare) DeleteCNAME(context.Context, string, string) error {
	f.deleteCount++
	return nil
}

type fakeDBService struct {
	nextID      int64
	attachments []dbengines.Attachment
	deleted     []int64
}

func (f *fakeDBService) AttachDatabase(_ context.Context, projectID, _ string, engine, envVarName string) (dbengines.Attachment, error) {
	f.nextID++
	attachment := dbengines.Attachment{
		ID:         f.nextID,
		ProjectID:  projectID,
		Engine:     engine,
		DBName:     "db_" + projectID,
		DBUser:     "user_" + projectID,
		Password:   "secret",
		EnvVarName: envVarName,
	}
	switch engine {
	case "mariadb":
		attachment.Host = "caddytower-mariadb"
		attachment.Port = 3306
	default:
		attachment.Host = "caddytower-postgres"
		attachment.Port = 5432
	}
	f.attachments = append(f.attachments, attachment)
	return attachment, nil
}

func (f *fakeDBService) ListAttachments(_ context.Context, projectID string) ([]dbengines.Attachment, error) {
	var attachments []dbengines.Attachment
	for _, attachment := range f.attachments {
		if attachment.ProjectID == projectID {
			attachments = append(attachments, attachment)
		}
	}
	return attachments, nil
}

func (f *fakeDBService) RotateAttachmentPassword(_ context.Context, attachmentID int64) (dbengines.Attachment, error) {
	for idx, attachment := range f.attachments {
		if attachment.ID == attachmentID {
			attachment.Password = "rotated"
			f.attachments[idx] = attachment
			return attachment, nil
		}
	}
	return dbengines.Attachment{}, store.ErrNotFound
}

func (f *fakeDBService) DeleteAttachment(_ context.Context, attachmentID int64) error {
	filtered := f.attachments[:0]
	found := false
	for _, attachment := range f.attachments {
		if attachment.ID == attachmentID {
			found = true
			continue
		}
		filtered = append(filtered, attachment)
	}
	if !found {
		return store.ErrNotFound
	}
	f.attachments = filtered
	f.deleted = append(f.deleted, attachmentID)
	return nil
}
