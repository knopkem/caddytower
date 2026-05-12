package github

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"caddytower/internal/config"
	"caddytower/internal/store"
)

func TestInstallationTokenCachesUntilExpiry(t *testing.T) {
	t.Parallel()

	keyPath := writeTestPrivateKey(t)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	var authHeaders []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		if r.URL.Path != "/app/installations/55/access_tokens" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"install-token","expires_at":"2026-05-12T10:10:00Z"}`))
	}))
	defer api.Close()

	stateStore := openGitHubTestStore(t)
	svc := New(Config{
		AppID:          12345,
		AppSlug:        "caddytower",
		PrivateKeyPath: keyPath,
		WebhookSecret:  "secret",
		APIBaseURL:     api.URL,
		WebBaseURL:     "https://github.example",
	}, stateStore, api.Client())
	svc.now = func() time.Time { return now }

	token1, err := svc.InstallationToken(context.Background(), 55)
	if err != nil {
		t.Fatalf("InstallationToken() error = %v", err)
	}
	token2, err := svc.InstallationToken(context.Background(), 55)
	if err != nil {
		t.Fatalf("InstallationToken() second error = %v", err)
	}

	if token1 != "install-token" || token2 != "install-token" {
		t.Fatalf("unexpected tokens %q / %q", token1, token2)
	}
	if len(authHeaders) != 1 {
		t.Fatalf("authHeaders = %d, want 1", len(authHeaders))
	}
	if !strings.HasPrefix(authHeaders[0], "Bearer ") {
		t.Fatalf("expected JWT bearer auth, got %q", authHeaders[0])
	}

	now = now.Add(10 * time.Minute)
	if _, err := svc.InstallationToken(context.Background(), 55); err != nil {
		t.Fatalf("InstallationToken() after expiry error = %v", err)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("authHeaders after expiry = %d, want 2", len(authHeaders))
	}
}

func TestHandleWebhookStoresAndDeletesInstallations(t *testing.T) {
	t.Parallel()

	stateStore := openGitHubTestStore(t)
	svc := New(Config{
		AppID:          12345,
		AppSlug:        "caddytower",
		PrivateKeyPath: "/unused/in/test",
		WebhookSecret:  "secret",
		APIBaseURL:     "https://api.github.test",
		WebBaseURL:     "https://github.test",
	}, stateStore, nil)

	createPayload := []byte(`{"action":"created","installation":{"id":42,"account":{"login":"example-org","type":"Organization"}}}`)
	if _, err := svc.HandleWebhook(context.Background(), "installation", signGitHubWebhook("secret", createPayload), createPayload); err != nil {
		t.Fatalf("HandleWebhook(create) error = %v", err)
	}

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(status.Installations) != 1 || status.Installations[0].AccountLogin != "example-org" {
		t.Fatalf("unexpected installations %#v", status.Installations)
	}

	deletePayload := []byte(`{"action":"deleted","installation":{"id":42,"account":{"login":"example-org","type":"Organization"}}}`)
	if _, err := svc.HandleWebhook(context.Background(), "installation", signGitHubWebhook("secret", deletePayload), deletePayload); err != nil {
		t.Fatalf("HandleWebhook(delete) error = %v", err)
	}

	status, err = svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() second error = %v", err)
	}
	if len(status.Installations) != 0 {
		t.Fatalf("expected installations to be deleted, got %#v", status.Installations)
	}
}

func TestVerifyWebhookSignatureRejectsInvalidSignatures(t *testing.T) {
	t.Parallel()

	svc := New(Config{
		AppID:          1,
		AppSlug:        "caddytower",
		PrivateKeyPath: "/unused/in/test",
		WebhookSecret:  "secret",
	}, nil, nil)
	if svc.VerifyWebhookSignature("sha256=bad", []byte(`{}`)) {
		t.Fatal("VerifyWebhookSignature() should reject invalid signature")
	}
}

func openGitHubTestStore(t *testing.T) *store.Store {
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

func writeTestPrivateKey(t *testing.T) string {
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

func signGitHubWebhook(secret string, payload []byte) string {
	mac := hmacSHA256(secret, payload)
	return "sha256=" + mac
}

func hmacSHA256(secret string, payload []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return fmt.Sprintf("%x", h.Sum(nil))
}
