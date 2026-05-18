package projects

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
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
	if len(caddy.managedHosts) != 2 || caddy.managedHosts[0] != "caddytower.example.com" || caddy.managedHosts[1] != "demo.example.com" {
		t.Fatalf("managed hosts = %#v", caddy.managedHosts)
	}
	if cloudflareFactory.client.upsertCount != 2 {
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

func TestSaveSettingsPreservesInstallerRootDomain(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	svc := New(config.Config{RootDomain: "pacsnode.com"}, stateStore, nil, docker, &fakeCaddy{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := svc.SaveSettings(context.Background(), SettingsInput{
		OriginHostname: "vps212846.vps.ovh.ca",
	}, ""); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	dashboard, err := svc.Dashboard(context.Background())
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}
	if dashboard.Settings.RootDomain != "pacsnode.com" {
		t.Fatalf("root domain = %q, want installer fallback", dashboard.Settings.RootDomain)
	}
}

func TestSaveSettingsReconcilesAdminRouteAndDNS(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	secretSvc, err := secrets.NewOptionalFromBase64("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	if err != nil {
		t.Fatalf("NewOptionalFromBase64() error = %v", err)
	}

	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	cloudflareFactory := &fakeCloudflareFactory{}
	svc := New(config.Config{RootDomain: "pacsnode.com"}, stateStore, secretSvc, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.newCloudflare = cloudflareFactory.New

	if err := svc.SaveSettings(context.Background(), SettingsInput{
		OriginHostname:    "203.0.113.10",
		CloudflareZoneID:  "zone-1",
		CloudflareToken:   "token-1",
		CloudflareProxied: true,
	}, ""); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	if len(caddy.managedHosts) != 1 || caddy.managedHosts[0] != "caddytower.pacsnode.com" {
		t.Fatalf("managed hosts = %#v", caddy.managedHosts)
	}
	if cloudflareFactory.client.upsertCount != 1 {
		t.Fatalf("upsertCount = %d", cloudflareFactory.client.upsertCount)
	}
}

func TestDashboardIncludesRequirementChecks(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{
		inspectByName: map[string]dockerx.ContainerInspect{
			"watchtower": {
				Name:    "watchtower",
				Running: true,
				Env:     []string{"DOCKER_API_VERSION=1.44"},
			},
		},
	}
	caddy := &fakeCaddy{}
	svc := New(config.Config{RootDomain: "example.com", CaddyAdminURL: "http://shared-caddy:2019"}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dashboard, err := svc.Dashboard(context.Background())
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}

	if !dashboard.Requirements.Available {
		t.Fatal("expected requirements to be available")
	}
	if dashboard.Requirements.HealthyCount != 3 || dashboard.Requirements.WarningCount != 0 || dashboard.Requirements.FailureCount != 0 {
		t.Fatalf("unexpected requirement counts: %#v", dashboard.Requirements)
	}
}

func TestDashboardFlagsBrokenRequirements(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{
		pingErr: errors.New("ping docker daemon: permission denied"),
	}
	caddy := &fakeCaddy{
		pingErr: errors.New("send ping request: dial tcp shared-caddy:2019: connect: connection refused"),
	}
	svc := New(config.Config{RootDomain: "example.com", CaddyAdminURL: "http://shared-caddy:2019"}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))

	dashboard, err := svc.Dashboard(context.Background())
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}

	if dashboard.Requirements.FailureCount != 2 || dashboard.Requirements.WarningCount != 1 {
		t.Fatalf("unexpected requirement counts: %#v", dashboard.Requirements)
	}
	if dashboard.Requirements.Checks[0].Summary != "CaddyTower cannot reach Docker right now." {
		t.Fatalf("docker summary = %q", dashboard.Requirements.Checks[0].Summary)
	}
	if dashboard.Requirements.Checks[1].Summary != "CaddyTower cannot reach the shared Caddy admin API." {
		t.Fatalf("caddy summary = %q", dashboard.Requirements.Checks[1].Summary)
	}
	if dashboard.Requirements.Checks[2].Summary != "Watchtower could not be checked because Docker is unavailable." {
		t.Fatalf("watchtower summary = %q", dashboard.Requirements.Checks[2].Summary)
	}
}

func TestSaveGitHubSettingsPersistsEncryptedRuntimeConfig(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	secretSvc, err := secrets.NewOptionalFromBase64("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	if err != nil {
		t.Fatalf("NewOptionalFromBase64() error = %v", err)
	}

	svc := New(config.Config{
		GitHubAPIBaseURL: "https://api.github.test",
		GitHubWebBaseURL: "https://github.test",
	}, stateStore, secretSvc, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := svc.SaveGitHubSettings(context.Background(), GitHubSettingsInput{
		AppID:         12345,
		AppSlug:       "caddytower",
		WebhookSecret: "github-secret",
		PrivateKeyPEM: generatedProjectsTestPrivateKeyPEM(t),
	}, ""); err != nil {
		t.Fatalf("SaveGitHubSettings() error = %v", err)
	}

	settings, err := svc.GitHubSettings(context.Background())
	if err != nil {
		t.Fatalf("GitHubSettings() error = %v", err)
	}
	if !settings.StoredInApp || !settings.Configured || settings.AppID != 12345 || settings.AppSlug != "caddytower" || settings.WebhookSecret != "github-secret" || settings.PrivateKeyPEM == "" {
		t.Fatalf("unexpected github settings %#v", settings)
	}

	raw, err := stateStore.GetSettings(context.Background(), settingGitHubWebhook, settingGitHubPrivateKey)
	if err != nil {
		t.Fatalf("GetSettings() error = %v", err)
	}
	if !strings.HasPrefix(raw[settingGitHubWebhook], "enc:") || !strings.HasPrefix(raw[settingGitHubPrivateKey], "enc:") {
		t.Fatalf("github settings should be encrypted at rest: %#v", raw)
	}

	githubService, err := svc.GitHubService(context.Background())
	if err != nil {
		t.Fatalf("GitHubService() error = %v", err)
	}
	if !githubService.Configured() {
		t.Fatal("expected runtime github service to be configured")
	}
}

func TestSelfUpdateControllerRecreatesControllerWithNewImage(t *testing.T) {
	t.Parallel()

	docker := &fakeDocker{
		inspectByName: map[string]dockerx.ContainerInspect{
			"caddytower": {
				Name:           "caddytower",
				Image:          "ghcr.io/knopkem/caddytower:v1.0.0",
				Command:        []string{"serve"},
				Networks:       []string{"edge"},
				Labels:         map[string]string{"com.centurylinklabs.watchtower.enable": "true"},
				Env:            []string{"CADDYTOWER_IMAGE=ghcr.io/knopkem/caddytower:v1.0.0", "CADDYTOWER_HTTP_ADDR=:8080"},
				Mounts:         []dockerx.Mount{{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}, {Source: "caddytower-data", Target: "/data"}},
				PublishedPorts: []dockerx.PortBinding{{ContainerPort: "8080", HostPort: "8080", HostIP: "127.0.0.1", Protocol: "tcp"}},
				RestartPolicy:  "unless-stopped",
			},
		},
	}

	err := SelfUpdateController(context.Background(), docker, "caddytower", "ghcr.io/knopkem/caddytower:v1.1.0")
	if err != nil {
		t.Fatalf("SelfUpdateController() error = %v", err)
	}
	if docker.pullCount != 1 || len(docker.pulledImages) != 1 || docker.pulledImages[0] != "ghcr.io/knopkem/caddytower:v1.1.0" {
		t.Fatalf("pulled images = %#v", docker.pulledImages)
	}
	if docker.lastSpec.Name != "caddytower" || docker.lastSpec.Image != "ghcr.io/knopkem/caddytower:v1.1.0" {
		t.Fatalf("lastSpec = %#v", docker.lastSpec)
	}
	if docker.lastSpec.Env["CADDYTOWER_IMAGE"] != "ghcr.io/knopkem/caddytower:v1.1.0" {
		t.Fatalf("updated env = %#v", docker.lastSpec.Env)
	}
	if docker.lastSpec.RestartPolicy != "unless-stopped" || docker.lastSpec.Network != "edge" {
		t.Fatalf("lastSpec = %#v", docker.lastSpec)
	}
}

func TestStartControllerUpdateLaunchesHelperContainer(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	svc := New(config.Config{}, stateStore, nil, docker, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := svc.StartControllerUpdate(context.Background(), "ghcr.io/knopkem/caddytower:v1.1.0", ""); err != nil {
		t.Fatalf("StartControllerUpdate() error = %v", err)
	}
	if docker.pullCount != 1 || docker.pulledImages[0] != "ghcr.io/knopkem/caddytower:v1.1.0" {
		t.Fatalf("pulled images = %#v", docker.pulledImages)
	}
	if !strings.HasPrefix(docker.lastSpec.Name, "caddytower-updater-") {
		t.Fatalf("helper name = %q", docker.lastSpec.Name)
	}
	if got := strings.Join(docker.lastSpec.Command, " "); !strings.Contains(got, "self-update") || !strings.Contains(got, "--target-image ghcr.io/knopkem/caddytower:v1.1.0") {
		t.Fatalf("helper command = %#v", docker.lastSpec.Command)
	}
	if !docker.lastSpec.AutoRemove {
		t.Fatalf("helper should auto-remove: %#v", docker.lastSpec)
	}
}

func TestRestartControllerRestartsManagedContainer(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	svc := New(config.Config{}, stateStore, nil, docker, &fakeCaddy{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := svc.RestartController(context.Background(), ""); err != nil {
		t.Fatalf("RestartController() error = %v", err)
	}
	if docker.restartCount != 1 {
		t.Fatalf("restart count = %d, want 1", docker.restartCount)
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

func TestRuntimeSnapshotWarnsWhenMemoryIsHigh(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{
		statsByName: map[string]dockerx.ContainerStatsSnapshot{
			"caddytower-demo": {
				ReadAt:           time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC),
				CPUPercent:       12.5,
				MemoryUsageBytes: 900,
				MemoryLimitBytes: 1000,
				MemoryPercent:    90,
				NetworkRxBytes:   2048,
				NetworkTxBytes:   4096,
			},
		},
	}
	svc := New(config.Config{RootDomain: "example.com"}, stateStore, nil, docker, &fakeCaddy{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

	snapshot, err := svc.RuntimeSnapshot(context.Background(), project)
	if err != nil {
		t.Fatalf("RuntimeSnapshot() error = %v", err)
	}
	if snapshot.MemoryPercent != 90 {
		t.Fatalf("MemoryPercent = %d", snapshot.MemoryPercent)
	}
	if len(snapshot.Warnings) == 0 || !strings.Contains(snapshot.Warnings[0], "Memory usage is close") {
		t.Fatalf("warnings = %#v", snapshot.Warnings)
	}
}

func TestCreateWebProjectStoresDeployHistoryAndRollbackPin(t *testing.T) {
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
	if len(project.Deploys) != 1 {
		t.Fatalf("deploys = %#v", project.Deploys)
	}
	if project.Deploys[0].Status != "live" || project.Deploys[0].ImageDigest == "" {
		t.Fatalf("deploy = %#v", project.Deploys[0])
	}

	rolledBack, err := svc.RollbackProject(context.Background(), project.ID, project.Deploys[0].ID, "")
	if err != nil {
		t.Fatalf("RollbackProject() error = %v", err)
	}
	if docker.lastSpec.Image != project.Deploys[0].ImageDigest {
		t.Fatalf("rollback image = %q, want %q", docker.lastSpec.Image, project.Deploys[0].ImageDigest)
	}
	if len(rolledBack.Deploys) < 2 || rolledBack.Deploys[0].Trigger != "rollback" {
		t.Fatalf("rollback deploys = %#v", rolledBack.Deploys)
	}
}

func TestAddAndVerifyProjectDomain(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	svc := New(config.Config{RootDomain: "example.com"}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.lookupCNAME = func(context.Context, string) (string, error) {
		return "origin.example.com.", nil
	}
	svc.lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"203.0.113.10"}, nil
	}

	if err := svc.SaveSettings(context.Background(), SettingsInput{
		RootDomain:     "example.com",
		OriginHostname: "origin.example.com",
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

	project, err = svc.AddProjectDomain(context.Background(), project.ID, "app.example.org", true, "")
	if err != nil {
		t.Fatalf("AddProjectDomain() error = %v", err)
	}
	if len(project.CustomDomains) != 1 || project.PrimaryDomain != "app.example.org" {
		t.Fatalf("project domains = %#v / primary=%q", project.CustomDomains, project.PrimaryDomain)
	}
	if len(caddy.managedHosts) != 3 {
		t.Fatalf("managed hosts = %#v", caddy.managedHosts)
	}

	project, err = svc.VerifyProjectDomain(context.Background(), project.ID, project.CustomDomains[0].ID, "")
	if err != nil {
		t.Fatalf("VerifyProjectDomain() error = %v", err)
	}
	if project.CustomDomains[0].DNSVerifiedAt.IsZero() {
		t.Fatalf("verified domain = %#v", project.CustomDomains[0])
	}
}

func TestRedeployHealthCheckFailureAutoRollsBack(t *testing.T) {
	t.Parallel()

	stateStore := openProjectsStore(t)
	docker := &fakeDocker{}
	caddy := &fakeCaddy{}
	svc := New(config.Config{RootDomain: "example.com"}, stateStore, nil, docker, caddy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.checkHTTP = func(context.Context, string, time.Duration) error { return nil }

	project, err := svc.CreateWebProject(context.Background(), WebProjectInput{
		Name:                      "Demo",
		Slug:                      "demo",
		ImageRef:                  "ghcr.io/example/demo:latest",
		Subdomain:                 "demo",
		InternalPort:              3000,
		HealthCheckPath:           "/ready",
		HealthCheckTimeoutSeconds: 2,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}
	initialPin := project.Deploys[0].ImageDigest

	attempts := 0
	svc.checkHTTP = func(context.Context, string, time.Duration) error {
		attempts++
		if attempts <= 3 {
			return errors.New("boom")
		}
		return nil
	}
	redeployed, err := svc.RedeployProject(context.Background(), project.ID, "")
	if err == nil || !strings.Contains(err.Error(), "automatically rolled back") {
		t.Fatalf("RedeployProject() error = %v, want auto-rollback failure", err)
	}

	current, _, getErr := svc.GetProject(context.Background(), project.ID)
	if getErr != nil {
		t.Fatalf("GetProject() error = %v", getErr)
	}
	if current.ImageRef != initialPin {
		t.Fatalf("current.ImageRef = %q, want %q", current.ImageRef, initialPin)
	}
	if len(current.Deploys) < 3 || current.Deploys[0].Trigger != "auto-rollback" {
		t.Fatalf("deploy history = %#v", current.Deploys)
	}
	if redeployed.ID != "" {
		t.Fatalf("redeployed = %#v, want zero value on failure", redeployed)
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
	restartCount  int
	removeCount   int
	pullCount     int
	pulledImages  []string
	lastSpec      dockerx.ContainerSpec
	logContent    string
	containers    []dockerx.ContainerSummary
	inspectByName map[string]dockerx.ContainerInspect
	inspectErrs   map[string]error
	statsByName   map[string]dockerx.ContainerStatsSnapshot
	pingErr       error
}

func (f *fakeDocker) Ping(context.Context) error {
	return f.pingErr
}

func (f *fakeDocker) PullImage(_ context.Context, image string) error {
	f.pullCount++
	f.pulledImages = append(f.pulledImages, image)
	return nil
}
func (f *fakeDocker) RecreateContainer(_ context.Context, spec dockerx.ContainerSpec) (dockerx.ContainerInspect, error) {
	f.recreateCount++
	f.lastSpec = spec
	return dockerx.ContainerInspect{
		ID:      uuid.NewString(),
		Name:    spec.Name,
		Image:   spec.Image,
		ImageID: "sha256:fake-image-" + strconv.Itoa(f.recreateCount),
		Running: true,
	}, nil
}
func (f *fakeDocker) InspectContainer(_ context.Context, name string) (dockerx.ContainerInspect, error) {
	if err, ok := f.inspectErrs[name]; ok {
		return dockerx.ContainerInspect{}, err
	}
	if inspect, ok := f.inspectByName[name]; ok {
		return inspect, nil
	}
	return dockerx.ContainerInspect{Running: true}, nil
}
func (f *fakeDocker) ContainerStats(_ context.Context, name string) (dockerx.ContainerStatsSnapshot, error) {
	if stats, ok := f.statsByName[name]; ok {
		return stats, nil
	}
	return dockerx.ContainerStatsSnapshot{}, nil
}
func (f *fakeDocker) ListContainersByLabel(context.Context, string, string) ([]dockerx.ContainerSummary, error) {
	return nil, nil
}
func (f *fakeDocker) ListContainers(context.Context) ([]dockerx.ContainerSummary, error) {
	return append([]dockerx.ContainerSummary(nil), f.containers...), nil
}
func (f *fakeDocker) RestartContainer(context.Context, string) error {
	f.restartCount++
	return nil
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
	pingErr      error
}

func (f *fakeCaddy) Ping(context.Context) error {
	return f.pingErr
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
func (f *fakeCloudflare) UpsertRecord(context.Context, string, string, string, bool) (cloudflare.DNSRecord, bool, error) {
	f.upsertCount++
	return cloudflare.DNSRecord{}, true, nil
}
func (f *fakeCloudflare) DeleteRecord(context.Context, string, string) error {
	f.deleteCount++
	return nil
}

func generatedProjectsTestPrivateKeyPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
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
