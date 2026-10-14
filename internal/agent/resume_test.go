package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
	"github.com/scottfridwin/offsite-backup-manager/internal/server"
)

const resumeRunID = "20260101T000000Z"

// publishRun writes a signed run whose package is pkg; declaredBytes
// overrides the manifest's size when non-zero.
func publishRun(t *testing.T, outputDir string, priv minisign.PrivateKey, pkg []byte, declaredBytes int64) {
	t.Helper()
	m := manifest.New(resumeRunID)
	m.Package = manifest.Package{
		Name: "run-" + resumeRunID + ".tar.zst.age", Bytes: int64(len(pkg)),
		SHA256: sha256Hex(pkg), Compression: "zstd", Encryption: "age",
	}
	if declaredBytes != 0 {
		m.Package.Bytes = declaredBytes
	}
	mb, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(outputDir, "run-"+resumeRunID)
	for name, b := range map[string][]byte{
		base + ".tar.zst.age":           pkg,
		base + ".manifest.json":         mb,
		base + ".manifest.json.minisig": minisign.Sign(priv, mb),
	} {
		if err := os.WriteFile(name, b, 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

type rangeRecorder struct {
	mu      sync.Mutex
	ranges  []string
	pkgHits int
}

// newTestAgent returns an enrolled agent talking to a real server handler,
// and records the Range header of every package request.
func newTestAgent(t *testing.T, outputDir, pubPath string) (*Agent, string, *rangeRecorder) {
	t.Helper()
	rosterStore := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	rec := &rangeRecorder{}
	h := server.NewHandler(server.Config{OutputDir: outputDir, Roster: rosterStore})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tar.zst.age") {
			rec.mu.Lock()
			rec.ranges = append(rec.ranges, r.Header.Get("Range"))
			rec.pkgHits++
			rec.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	tok, err := rosterStore.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{BackupHost: "example.invalid", EnrollmentToken: tok, NodeLabel: "pi", StoreDir: t.TempDir(), MinisignPubKeyFile: pubPath})
	if err != nil {
		t.Fatal(err)
	}
	a.baseURL, a.client, a.dlClient = ts.URL, ts.Client(), ts.Client()
	pullToken, err := a.EnsureEnrolled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a, pullToken, rec
}

func TestAgentResumesPartialDownload(t *testing.T) {
	pubPath, priv := writeMinisignKeyPair(t)
	out := t.TempDir()
	pkg := []byte(strings.Repeat("0123456789", 1000))
	publishRun(t, out, priv, pkg, 0)
	a, tok, rec := newTestAgent(t, out, pubPath)

	partial := a.incomingPath("run-" + resumeRunID + ".tar.zst.age.partial")
	if err := os.WriteFile(partial, pkg[:4000], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunOnce(context.Background(), tok); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(a.cfg.StoreDir, "run-"+resumeRunID+".tar.zst.age"))
	if err != nil || string(got) != string(pkg) {
		t.Fatalf("stored package wrong (%d bytes): %v", len(got), err)
	}
	if len(rec.ranges) != 1 || rec.ranges[0] != "bytes=4000-" {
		t.Fatalf("package requests used ranges %q, want one resume from 4000", rec.ranges)
	}
	if fileExists(partial) {
		t.Fatal("partial file left after commit")
	}
}

func TestAgentDiscardsCorruptPartial(t *testing.T) {
	pubPath, priv := writeMinisignKeyPair(t)
	out := t.TempDir()
	pkg := []byte(strings.Repeat("abcdefghij", 500))
	publishRun(t, out, priv, pkg, 0)
	a, tok, _ := newTestAgent(t, out, pubPath)

	partial := a.incomingPath("run-" + resumeRunID + ".tar.zst.age.partial")
	if err := os.WriteFile(partial, []byte(strings.Repeat("X", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunOnce(context.Background(), tok); err == nil {
		t.Fatal("expected a checksum mismatch from the corrupt partial")
	}
	if fileExists(partial) {
		t.Fatal("corrupt partial must be discarded")
	}
	if _, err := a.RunOnce(context.Background(), tok); err != nil {
		t.Fatalf("second RunOnce should download afresh: %v", err)
	}
}

func TestAgentRefusesWhenStoreTooSmall(t *testing.T) {
	pubPath, priv := writeMinisignKeyPair(t)
	out := t.TempDir()
	publishRun(t, out, priv, []byte("small"), 1<<60)
	a, tok, rec := newTestAgent(t, out, pubPath)

	_, err := a.RunOnce(context.Background(), tok)
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("RunOnce error = %v, want not enough free space", err)
	}
	if rec.pkgHits != 0 {
		t.Fatalf("package was requested %d times despite too little space", rec.pkgHits)
	}
}

func TestAgentFinishesHalfStoredRunWithoutRedownload(t *testing.T) {
	pubPath, priv := writeMinisignKeyPair(t)
	out := t.TempDir()
	pkg := []byte("ciphertext")
	publishRun(t, out, priv, pkg, 0)
	a, tok, rec := newTestAgent(t, out, pubPath)

	// Simulate a crash after the package was stored but before its manifest.
	stored := filepath.Join(a.cfg.StoreDir, "run-"+resumeRunID+".tar.zst.age")
	if err := os.WriteFile(stored, pkg, 0o400); err != nil {
		t.Fatal(err)
	}
	last, err := a.RunOnce(context.Background(), tok)
	if err != nil || last != resumeRunID {
		t.Fatalf("RunOnce = %q, %v", last, err)
	}
	if rec.pkgHits != 0 {
		t.Fatalf("package re-downloaded %d times", rec.pkgHits)
	}
	for _, suffix := range []string{".manifest.json", ".manifest.json.minisig"} {
		if !fileExists(filepath.Join(a.cfg.StoreDir, "run-"+resumeRunID+suffix)) {
			t.Errorf("missing %s", suffix)
		}
	}
}

func TestAgentStartupCleansIncoming(t *testing.T) {
	pubPath, _ := writeMinisignKeyPair(t)
	store := t.TempDir()
	incoming := filepath.Join(store, incomingDirName)
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(incoming, "write-123.tmp"):                            "stale",
		filepath.Join(incoming, "run-20260101T000000Z.tar.zst.age.partial"): "resume me",
		filepath.Join(incoming, "run-20250101T000000Z.tar.zst.age.partial"): "already stored",
		filepath.Join(store, "run-20250101T000000Z.tar.zst.age"):            "stored",
		filepath.Join(store, ".download-42.tmp"):                            "legacy",
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := New(Config{BackupHost: "example.invalid", StoreDir: store, MinisignPubKeyFile: pubPath})
	if err != nil {
		t.Fatal(err)
	}

	for p, keep := range map[string]bool{
		filepath.Join(incoming, "write-123.tmp"):                            false,
		filepath.Join(incoming, "run-20260101T000000Z.tar.zst.age.partial"): true,
		filepath.Join(incoming, "run-20250101T000000Z.tar.zst.age.partial"): false,
		filepath.Join(store, "run-20250101T000000Z.tar.zst.age"):            true,
		filepath.Join(store, ".download-42.tmp"):                            false,
	} {
		if fileExists(p) != keep {
			t.Errorf("%s: exists=%v, want %v", p, !keep, keep)
		}
	}
	if got, _ := a.lastRunID.Load().(string); got != "20250101T000000Z" {
		t.Errorf("lastRunID = %q, want the newest stored run", got)
	}
}
