package projects

import (
	"context"
	"io"
	"log/slog"
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
		Name:              "Books",
		Slug:              "books",
		ImageRef:          "ghcr.io/example/books:latest",
		Subdomain:         "books",
		InternalPort:      3000,
		WatchtowerEnabled: true,
		EnvText:           "NODE_ENV=production\nAPI_KEY=secret",
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	if project.FullDomain != "books.example.com" {
		t.Fatalf("project.FullDomain = %q", project.FullDomain)
	}
	if docker.recreateCount != 1 {
		t.Fatalf("recreateCount = %d", docker.recreateCount)
	}
	if len(caddy.managedHosts) != 1 || caddy.managedHosts[0] != "books.example.com" {
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
		Name:         "Books",
		Slug:         "books",
		ImageRef:     "ghcr.io/example/books:latest",
		Subdomain:    "books",
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
		Name:         "Books",
		Slug:         "books",
		ImageRef:     "ghcr.io/example/books:latest",
		Subdomain:    "books",
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
		Name:         "Books",
		Slug:         "books",
		ImageRef:     "ghcr.io/example/books:latest",
		Subdomain:    "books",
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
		Name:         "Books",
		Slug:         "books",
		ImageRef:     "ghcr.io/example/books:latest",
		Subdomain:    "books",
		InternalPort: 3000,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	redeployed, err := svc.RedeployProjectByWebhook(context.Background(), "books")
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
}

func (f *fakeDocker) PullImage(context.Context, string) error { return nil }
func (f *fakeDocker) RecreateContainer(_ context.Context, spec dockerx.ContainerSpec) (dockerx.ContainerInspect, error) {
	f.recreateCount++
	return dockerx.ContainerInspect{
		ID:      uuid.NewString(),
		Name:    spec.Name,
		Image:   spec.Image,
		Running: true,
	}, nil
}
func (f *fakeDocker) InspectContainer(context.Context, string) (dockerx.ContainerInspect, error) {
	return dockerx.ContainerInspect{Running: true}, nil
}
func (f *fakeDocker) ListContainersByLabel(context.Context, string, string) ([]dockerx.ContainerSummary, error) {
	return nil, nil
}
func (f *fakeDocker) RemoveContainer(context.Context, string) error {
	f.removeCount++
	return nil
}

type fakeCaddy struct {
	managedHosts []string
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
