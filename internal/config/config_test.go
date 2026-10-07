package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSecretFromEnvLiteral(t *testing.T) {
	t.Setenv("TEST_SECRET", "literal-value")
	v, err := SecretFromEnv("TEST_SECRET")
	if err != nil {
		t.Fatalf("secretFromEnv: %v", err)
	}
	if v != "literal-value" {
		t.Fatalf("got %q, want %q", v, "literal-value")
	}
}

func TestSecretFromEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	t.Setenv("TEST_SECRET", "literal-should-be-ignored")
	t.Setenv("TEST_SECRET_FILE", path)

	v, err := SecretFromEnv("TEST_SECRET")
	if err != nil {
		t.Fatalf("secretFromEnv: %v", err)
	}
	if v != "from-file" {
		t.Fatalf("got %q, want %q (trimmed)", v, "from-file")
	}
}

func TestSecretFromEnvMissingFile(t *testing.T) {
	t.Setenv("TEST_SECRET_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := SecretFromEnv("TEST_SECRET"); err == nil {
		t.Fatal("expected error for missing secret file")
	}
}

func TestSecretFromEnvUnset(t *testing.T) {
	v, err := SecretFromEnv("TEST_SECRET_NOT_SET")
	if err != nil {
		t.Fatalf("secretFromEnv: %v", err)
	}
	if v != "" {
		t.Fatalf("got %q, want empty", v)
	}
}
