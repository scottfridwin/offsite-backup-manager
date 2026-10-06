package health

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

func writeRun(t *testing.T, outputDir string, m manifest.Manifest) {
	t.Helper()
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	base := "run-" + m.RunID
	files := map[string][]byte{
		base + ".tar.zst.age":           []byte("ciphertext"),
		base + ".manifest.json":         b,
		base + ".manifest.json.minisig": []byte("signature"),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(outputDir, name), content, 0o640); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func newManifestAt(runID string, createdAt time.Time, scheme string, assigned []string) manifest.Manifest {
	m := manifest.New(runID)
	m.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	m.Distribution = manifest.Distribution{Scheme: scheme, AssignedNodeIDs: assigned}
	return m
}

func TestEvaluateNoRunsYet(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))

	res, err := Evaluate(Config{OutputDir: outputDir, Roster: store, RunInterval: time.Hour, SyncGrace: time.Minute, SpaceCriticalPct: 90})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy with no runs")
	}
}

func TestEvaluateHealthyWithinGrace(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	tok, _ := store.IssueEnrollmentToken(time.Hour)
	node, _, err := store.Enroll(tok, "pi-garage")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, newManifestAt("20260101T000000Z", now.Add(-time.Hour), manifest.SchemeReplicateAll, nil))

	// Node hasn't confirmed yet, but we're still within the sync grace
	// window, so this must still be healthy (eventual consistency).
	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		RunInterval: 24 * time.Hour, SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy within grace window, got reasons: %v", res.Reasons)
	}
	_ = node
}

func TestEvaluateUnhealthyAfterGraceWithoutConfirmation(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	tok, _ := store.IssueEnrollmentToken(time.Hour)
	if _, _, err := store.Enroll(tok, "pi-garage"); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, newManifestAt("20260101T000000Z", now.Add(-7*time.Hour), manifest.SchemeReplicateAll, nil))

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		RunInterval: 24 * time.Hour, SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy once grace window elapses without confirmation")
	}
}

func TestEvaluateHealthyAfterConfirmation(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	tok, _ := store.IssueEnrollmentToken(time.Hour)
	node, _, err := store.Enroll(tok, "pi-garage")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := store.Heartbeat(node.ID, 100, 200, "20260101T000000Z"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, newManifestAt("20260101T000000Z", now.Add(-7*time.Hour), manifest.SchemeReplicateAll, nil))

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		RunInterval: 24 * time.Hour, SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy once node confirmed, got reasons: %v", res.Reasons)
	}
}

func TestEvaluateUnhealthyStaleRun(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))

	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, newManifestAt("20260101T000000Z", now.Add(-9*24*time.Hour), manifest.SchemeReplicateAll, nil))

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		RunInterval: 48 * time.Hour, SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy for a stale run")
	}
}

func TestEvaluateRoundRobinOnlyRequiresAssignedNode(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	tokA, _ := store.IssueEnrollmentToken(time.Hour)
	nodeA, _, err := store.Enroll(tokA, "a")
	if err != nil {
		t.Fatalf("enroll a: %v", err)
	}
	tokB, _ := store.IssueEnrollmentToken(time.Hour)
	if _, _, err := store.Enroll(tokB, "b"); err != nil {
		t.Fatalf("enroll b: %v", err)
	}
	if err := store.Heartbeat(nodeA.ID, 100, 200, "20260101T000000Z"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, newManifestAt("20260101T000000Z", now.Add(-7*time.Hour), manifest.SchemeRoundRobin, []string{nodeA.ID}))

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		RunInterval: 24 * time.Hour, SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// Node B was never assigned this run, so its silence must not matter.
	if !res.Healthy {
		t.Fatalf("expected healthy (only the assigned node matters), got reasons: %v", res.Reasons)
	}
}

func TestEvaluateCriticalSpace(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	tok, _ := store.IssueEnrollmentToken(time.Hour)
	node, _, err := store.Enroll(tok, "pi-garage")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// 95% used.
	if err := store.Heartbeat(node.ID, 5, 100, "20260101T000000Z"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, newManifestAt("20260101T000000Z", now.Add(-time.Minute), manifest.SchemeReplicateAll, nil))

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		RunInterval: 24 * time.Hour, SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy due to critical node space")
	}
}
