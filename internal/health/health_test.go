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

func TestEvaluateNoRunsNoScheduleIsHealthy(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))

	// No runs and no schedule: no cadence is known, so nothing can be missed.
	res, err := Evaluate(Config{OutputDir: outputDir, Roster: store, RunInterval: time.Hour, SyncGrace: time.Minute, SpaceCriticalPct: 90})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected a fresh primary with no schedule to be healthy, got: %v", res.Reasons)
	}
}

func TestEvaluateMissingOutputDirIsHealthy(t *testing.T) {
	// A freshly deployed primary whose output dir doesn't exist yet must not
	// error; it's simply "no runs yet".
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))

	res, err := Evaluate(Config{OutputDir: missing, Roster: store, SyncGrace: time.Minute, SpaceCriticalPct: 90})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy when output dir is absent, got: %v", res.Reasons)
	}
}

func TestEvaluateNoRunsBeforeFirstScheduledRunIsHealthy(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))

	deploy := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(outputDir, deploy, deploy); err != nil {
		t.Fatal(err)
	}
	now := deploy.Add(time.Hour) // first daily 12:00 run isn't due yet

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		Schedule: "0 12 * * *", SyncGrace: time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy before the first scheduled run, got: %v", res.Reasons)
	}
}

func TestEvaluateNoRunsAfterMissedFirstScheduledRunIsUnhealthy(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))

	deploy := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(outputDir, deploy, deploy); err != nil {
		t.Fatal(err)
	}
	now := deploy.Add(25 * time.Hour) // well past the first 12:00 run + grace

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		Schedule: "0 12 * * *", SyncGrace: time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy once the first scheduled run is missed")
	}
}

func TestEvaluateScheduleToleratesIntervalBetweenRuns(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))

	// Monthly schedule; the last run is 14 days old (well past the 48h fixed
	// interval), but the next scheduled run isn't due yet, so it's healthy.
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, newManifestAt("20260101T120000Z", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), manifest.SchemeReplicateAll, nil))

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		Schedule: "0 12 1 * *", RunInterval: 48 * time.Hour, SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy between scheduled monthly runs, got: %v", res.Reasons)
	}
}

func TestEvaluateScheduleUnhealthyWhenRunMissed(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))

	// Daily schedule; the last run is 2 days old, so a scheduled run was missed.
	now := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, newManifestAt("20260101T120000Z", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), manifest.SchemeReplicateAll, nil))

	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store,
		Schedule: "0 12 * * *", RunInterval: 48 * time.Hour, SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy when a scheduled run was missed")
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
	node, _, err := store.Enroll(tok, "pi-garage")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// The node is a real participant (it confirmed an earlier run) but has not
	// confirmed the latest one, so after grace the Primary must be unhealthy.
	if err := store.Heartbeat(node.ID, 100, 200, "20251231T000000Z"); err != nil {
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
	if res.Healthy {
		t.Fatal("expected unhealthy once grace window elapses without confirmation")
	}
}

// A node that enrolled but never heartbeated is not a confirmed participant and
// must not keep the Primary unhealthy forever (#16).
func TestEvaluatePhantomNodeDoesNotGateHealth(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	tok, _ := store.IssueEnrollmentToken(time.Hour)
	if _, _, err := store.Enroll(tok, "phantom"); err != nil {
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
	if !res.Healthy {
		t.Fatalf("expected healthy (a never-confirmed node must not gate health), got reasons: %v", res.Reasons)
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
