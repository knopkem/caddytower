package auth

import (
	"context"
	"testing"
	"time"

	"caddytower/internal/config"
	"caddytower/internal/store"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

func TestBootstrapAndLoginFlow(t *testing.T) {
	t.Parallel()

	stateStore := openTestStore(t)
	svc := New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedNow }

	ctx := context.Background()
	enrollment, err := svc.GenerateEnrollment("admin@example.com")
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

	sessionToken, user, err := svc.CreateInitialUser(ctx, "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	if user.Email != "admin@example.com" {
		t.Fatalf("user.Email = %q", user.Email)
	}

	authenticated, err := svc.Authenticate(ctx, sessionToken)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if authenticated.Email != "admin@example.com" {
		t.Fatalf("authenticated.Email = %q", authenticated.Email)
	}

	if err := svc.Logout(ctx, sessionToken); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}

	loginCode, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}

	secondToken, loggedInUser, err := svc.Login(ctx, "admin@example.com", "super-secure-password", loginCode, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if secondToken == "" {
		t.Fatal("expected session token")
	}
	if loggedInUser.Email != "admin@example.com" {
		t.Fatalf("loggedInUser.Email = %q", loggedInUser.Email)
	}
}

func TestLoginLocksAccountAfterRepeatedFailures(t *testing.T) {
	t.Parallel()

	stateStore := openTestStore(t)
	svc := New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedNow }
	svc.ipLimit = 100

	ctx := context.Background()
	enrollment, err := svc.GenerateEnrollment("admin@example.com")
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

	if _, _, err := svc.CreateInitialUser(ctx, "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent"); err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	for i := 0; i < svc.lockoutAfter-1; i++ {
		if _, _, err := svc.Login(ctx, "admin@example.com", "wrong-password", code, "127.0.0.1", "test-agent"); err == nil {
			t.Fatalf("expected login failure on attempt %d", i+1)
		}
	}

	if _, _, err := svc.Login(ctx, "admin@example.com", "wrong-password", code, "127.0.0.1", "test-agent"); err != ErrAccountLocked {
		t.Fatalf("expected ErrAccountLocked, got %v", err)
	}
}

func openTestStore(t *testing.T) *store.Store {
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

	t.Cleanup(func() {
		_ = stateStore.Close()
	})

	return stateStore
}
