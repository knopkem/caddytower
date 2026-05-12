package secrets

import (
	"encoding/base64"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}

	svc, err := NewFromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewFromBase64() error = %v", err)
	}

	encrypted, err := svc.EncryptString("postgres://user:pass@db/service")
	if err != nil {
		t.Fatalf("EncryptString() error = %v", err)
	}

	decrypted, err := svc.DecryptString(encrypted)
	if err != nil {
		t.Fatalf("DecryptString() error = %v", err)
	}

	if decrypted != "postgres://user:pass@db/service" {
		t.Fatalf("decrypted = %q", decrypted)
	}
}

func TestNewFromBase64RejectsShortKey(t *testing.T) {
	t.Parallel()

	_, err := NewFromBase64(base64.StdEncoding.EncodeToString([]byte("short")))
	if err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestNewOptionalFromBase64Empty(t *testing.T) {
	t.Parallel()

	svc, err := NewOptionalFromBase64("")
	if err != nil {
		t.Fatalf("NewOptionalFromBase64() error = %v", err)
	}

	if svc != nil {
		t.Fatal("expected nil service for empty key")
	}
}
