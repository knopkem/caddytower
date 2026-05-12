package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"caddytower/internal/auth"
	"caddytower/internal/config"
	githubapp "caddytower/internal/github"
	"caddytower/internal/projects"
	"caddytower/internal/store"
	"caddytower/internal/ui"
	"caddytower/internal/version"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

func TestImportPageShowsRepositoryAnalysis(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService, sessionToken := issueImportTestSession(t, stateStore)
	githubService, api := newImportGitHubTestService(t, stateStore)
	defer api.Close()
	registerGitHubInstallation(t, githubService)

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, nil, nil, newNoopLogger())

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, githubService, nil)
	srv.imageChecker = &imageRefChecker{
		client: api.Client(),
		baseURLForRegistry: func(string) string {
			return api.URL
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/import?installation=42&repo=example-org/demo", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: sessionToken})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "example-org/demo") || !strings.Contains(body, "ghcr.io/example-org/demo:latest") {
		t.Fatalf("body missing repository analysis: %q", body)
	}
	if !strings.Contains(body, "Workflow PR") || !strings.Contains(body, "Create project") {
		t.Fatalf("body missing import wizard actions: %q", body)
	}
}

func TestImportCreateCreatesProjectAndWorkflowPR(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService, sessionToken := issueImportTestSession(t, stateStore)
	githubService, api := newImportGitHubTestService(t, stateStore)
	defer api.Close()
	registerGitHubInstallation(t, githubService)

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, nil, nil, newNoopLogger())

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, githubService, nil)
	srv.imageChecker = &imageRefChecker{
		client: api.Client(),
		baseURLForRegistry: func(string) string {
			return api.URL
		},
	}

	form := url.Values{
		"csrf_token":      {"import-csrf"},
		"installation_id": {"42"},
		"repo_full_name":  {"example-org/demo"},
		"project_type":    {"web"},
		"name":            {"Demo"},
		"slug":            {"demo"},
		"subdomain":       {"demo"},
		"image_ref":       {"ghcr.io/example-org/demo:latest"},
		"internal_port":   {"3000"},
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/import", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: "caddytower_csrf", Value: "import-csrf"})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); !strings.Contains(got, "/projects/") {
		t.Fatalf("redirect location = %q", got)
	}

	dashboard, err := projectService.Dashboard(context.Background())
	if err != nil {
		t.Fatalf("Dashboard() error = %v", err)
	}
	if len(dashboard.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(dashboard.Projects))
	}
	project := dashboard.Projects[0]
	if project.Status != "pending image" {
		t.Fatalf("project status = %q, want pending image", project.Status)
	}
	if project.GitHubRepoFullName != "example-org/demo" || project.GitHubInstallationID != 42 || project.GitHubDefaultBranch != "main" {
		t.Fatalf("unexpected GitHub linkage %#v", project)
	}
}

func issueImportTestSession(t *testing.T, stateStore *store.Store) (*auth.Service, string) {
	t.Helper()

	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })

	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}
	return authService, token
}

func registerGitHubInstallation(t *testing.T, githubService *githubapp.Service) {
	t.Helper()

	payload := []byte(`{"action":"created","installation":{"id":42,"account":{"login":"example-org","type":"Organization"}}}`)
	if _, err := githubService.HandleWebhook(context.Background(), "installation", testWebhookSignature("github-secret", payload), payload); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}
}

func newImportGitHubTestService(t *testing.T, stateStore *store.Store) (*githubapp.Service, *httptest.Server) {
	t.Helper()

	keyPath := writeImportTestPrivateKey(t)
	api := httptest.NewServer(newImportGitHubHandler())
	service := githubapp.New(githubapp.Config{
		AppID:          12345,
		AppSlug:        "caddytower",
		PrivateKeyPath: keyPath,
		WebhookSecret:  "github-secret",
		APIBaseURL:     api.URL,
		WebBaseURL:     "https://github.test",
	}, stateStore, api.Client())
	return service, api
}

func newImportGitHubHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"install-token","expires_at":"2026-05-12T10:10:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":[{"id":1,"name":"demo","full_name":"example-org/demo","default_branch":"main","private":false,"html_url":"https://github.test/example-org/demo"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/example-org/demo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":1,"name":"demo","full_name":"example-org/demo","default_branch":"main","private":false,"html_url":"https://github.test/example-org/demo"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/example-org/demo/contents/Dockerfile":
			writeGitHubContentFile(w, "Dockerfile", "FROM caddy\nEXPOSE 3000\n")
		case r.Method == http.MethodGet && r.URL.Path == "/repos/example-org/demo/contents/.github/workflows":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/example-org/demo/contents/"):
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/example-org/demo/git/ref/heads/main":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":{"sha":"abc123"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/example-org/demo/git/refs":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/example-org/demo/contents/.github/workflows/caddytower-deploy.yml":
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			content, _ := base64.StdEncoding.DecodeString(payload["content"])
			text := string(content)
			if !strings.Contains(text, "/api/webhooks/deploy/demo") || !strings.Contains(text, importWorkflowSecretName) {
				http.Error(w, "workflow missing webhook config", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/example-org/demo/pulls":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.test/example-org/demo/pull/7"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/example-org/demo/manifests/latest"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func writeGitHubContentFile(w http.ResponseWriter, name, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"name":     name,
		"type":     "file",
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString([]byte(content)),
	})
}

func writeImportTestPrivateKey(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := filepath.Join(t.TempDir(), "github-app.pem")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}
