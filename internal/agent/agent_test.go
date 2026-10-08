package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
	"github.com/scottfridwin/offsite-backup-manager/internal/server"
)

// writeMinisignPubFile writes a minisign public key in the on-disk text
// format LoadVerifier expects and returns its path plus the matching signer.
func writeMinisignKeyPair(t *testing.T) (pubPath string, priv minisign.PrivateKey) {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate minisign key: %v", err)
	}
	pubText, err := pub.MarshalText()
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPath = filepath.Join(t.TempDir(), "minisign.pub")
	if err := os.WriteFile(pubPath, pubText, 0o640); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	return pubPath, priv
}

func writeServerRun(t *testing.T, outputDir, runID string, priv minisign.PrivateKey) {
	t.Helper()
	pkgBytes := []byte("ciphertext-for-" + runID)

	m := manifest.New(runID)
	m.Package = manifest.Package{
		Name:        "run-" + runID + ".tar.zst.age",
		Bytes:       int64(len(pkgBytes)),
		SHA256:      sha256Hex(pkgBytes),
		Compression: "zstd",
		Encryption:  "age",
	}
	mb, err := m.Marshal()
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	sig := minisign.Sign(priv, mb)

	files := map[string][]byte{
		"run-" + runID + ".tar.zst.age":           pkgBytes,
		"run-" + runID + ".manifest.json":         mb,
		"run-" + runID + ".manifest.json.minisig": sig,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(outputDir, name), content, 0o640); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestAgentEnrollPullHeartbeat(t *testing.T) {
	pubPath, priv := writeMinisignKeyPair(t)

	outputDir := t.TempDir()
	rosterStore := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	handler := server.NewHandler(server.Config{OutputDir: outputDir, Roster: rosterStore})
	ts := httptest.NewTLSServer(handler)
	defer ts.Close()

	writeServerRun(t, outputDir, "20260101T000000Z", priv)

	enrollTok, err := rosterStore.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatalf("issue enrollment token: %v", err)
	}

	a, err := New(Config{
		BackupHost:         "example.invalid", // overridden below; required for validation only
		EnrollmentToken:    enrollTok,
		NodeLabel:          "pi-garage",
		StoreDir:           t.TempDir(),
		MinisignPubKeyFile: pubPath,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	// Point the agent at the test TLS server instead of a real BACKUP_HOST.
	a.baseURL = ts.URL
	a.client = ts.Client()
	a.dlClient = ts.Client()

	ctx := context.Background()
	pullToken, err := a.EnsureEnrolled(ctx)
	if err != nil {
		t.Fatalf("ensure enrolled: %v", err)
	}
	if pullToken == "" {
		t.Fatal("expected a non-empty pull token")
	}

	// Re-calling EnsureEnrolled must reuse the persisted credential, not
	// re-enroll (the bootstrap token is single-use).
	again, err := a.EnsureEnrolled(ctx)
	if err != nil {
		t.Fatalf("ensure enrolled (cached): %v", err)
	}
	if again != pullToken {
		t.Fatalf("expected cached credential reuse, got different token")
	}

	lastRunID, err := a.RunOnce(ctx, pullToken)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if lastRunID != "20260101T000000Z" {
		t.Fatalf("unexpected last run id: %q", lastRunID)
	}

	pkgPath := filepath.Join(a.cfg.StoreDir, "run-20260101T000000Z.tar.zst.age")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		t.Fatalf("read stored package: %v", err)
	}
	if string(data) != "ciphertext-for-20260101T000000Z" {
		t.Fatalf("unexpected stored package content: %q", data)
	}

	// Re-running must not re-fetch or error (already present).
	again2, err := a.RunOnce(ctx, pullToken)
	if err != nil {
		t.Fatalf("second run once: %v", err)
	}
	if again2 != "20260101T000000Z" {
		t.Fatalf("expected idempotent re-run to report same run id, got %q", again2)
	}

	// Stored artifacts are read-only (best-effort WORM enforcement).
	fi, err := os.Stat(pkgPath)
	if err != nil {
		t.Fatalf("stat package: %v", err)
	}
	if fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("expected stored package to be read-only, got mode %v", fi.Mode())
	}

	if err := a.Heartbeat(ctx, pullToken, lastRunID); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	active, err := rosterStore.ActiveNodes()
	if err != nil {
		t.Fatalf("active nodes: %v", err)
	}
	if len(active) != 1 || active[0].LastRunID != "20260101T000000Z" {
		t.Fatalf("unexpected active nodes after heartbeat: %+v", active)
	}
}

func TestAgentRejectsTamperedManifest(t *testing.T) {
	pubPath, _ := writeMinisignKeyPair(t)
	// Sign with a different key than pubPath so verification fails.
	_, otherPriv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}

	outputDir := t.TempDir()
	rosterStore := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	handler := server.NewHandler(server.Config{OutputDir: outputDir, Roster: rosterStore})
	ts := httptest.NewTLSServer(handler)
	defer ts.Close()

	writeServerRun(t, outputDir, "20260101T000000Z", otherPriv)

	enrollTok, err := rosterStore.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatalf("issue enrollment token: %v", err)
	}

	a, err := New(Config{
		BackupHost:         "example.invalid",
		EnrollmentToken:    enrollTok,
		NodeLabel:          "pi-garage",
		StoreDir:           t.TempDir(),
		MinisignPubKeyFile: pubPath,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	a.baseURL = ts.URL
	a.client = ts.Client()
	a.dlClient = ts.Client()
	ctx := context.Background()
	pullToken, err := a.EnsureEnrolled(ctx)
	if err != nil {
		t.Fatalf("ensure enrolled: %v", err)
	}

	if _, err := a.RunOnce(ctx, pullToken); err == nil {
		t.Fatal("expected signature verification failure")
	}

	pkgPath := filepath.Join(a.cfg.StoreDir, "run-20260101T000000Z.tar.zst.age")
	if fileExists(pkgPath) {
		t.Fatal("unverified package must not be stored")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
