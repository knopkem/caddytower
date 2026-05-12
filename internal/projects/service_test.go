package projects

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"caddytower/internal/caddyadmin"
	"caddytower/internal/cloudflare"
	"caddytower/internal/config"
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
