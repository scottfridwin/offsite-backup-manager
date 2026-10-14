package health

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

func TestEvaluateFailedRunIsUnhealthyUntilASuccess(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	now := time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)
	cfg := Config{
		OutputDir: outputDir, Roster: store, Schedule: "0 12 1 * *",
		SyncGrace: 6 * time.Hour, SpaceCriticalPct: 90, Now: func() time.Time { return now },
	}

	// Last month's run is fine; today's scheduled run just failed.
	writeRun(t, outputDir, newManifestAt("20260201T120000Z", now.AddDate(0, -1, 0), manifest.SchemeReplicateAll, nil))
	if err := WriteRunStatus(outputDir, RunStatus{FinishedAt: now.Add(-10 * time.Minute), Error: "stage: disk full"}); err != nil {
		t.Fatal(err)
	}
	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy right after a failed run, without waiting for the grace window")
	}

	// A later successful run clears it.
	writeRun(t, outputDir, newManifestAt("20260301T122500Z", now.Add(-5*time.Minute), manifest.SchemeReplicateAll, nil))
	if err := WriteRunStatus(outputDir, RunStatus{FinishedAt: now.Add(-5 * time.Minute), RunID: "20260301T122500Z"}); err != nil {
		t.Fatal(err)
	}
	if res, err = Evaluate(cfg); err != nil || !res.Healthy {
		t.Fatalf("expected healthy after a successful run, got %v %v", res.Reasons, err)
	}
}

func TestEvaluateFailedFirstRunIsUnhealthy(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	if err := WriteRunStatus(outputDir, RunStatus{FinishedAt: time.Now(), Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	res, err := Evaluate(Config{OutputDir: outputDir, Roster: store, Schedule: "0 12 1 * *", SyncGrace: time.Hour, SpaceCriticalPct: 90})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy when the first run failed")
	}
}

func TestEvaluateNoNodesIsHealthy(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	created := time.Now().Add(-30 * 24 * time.Hour)
	writeRun(t, outputDir, newManifestAt("20260101T120000Z", created, manifest.SchemeReplicateAll, nil))

	// No schedule-driven miss (RunInterval covers it) and no nodes: a deployed
	// Primary without nodes yet must stay green.
	res, err := Evaluate(Config{
		OutputDir: outputDir, Roster: store, RunInterval: 60 * 24 * time.Hour,
		SyncGrace: time.Hour, SpaceCriticalPct: 90, NodeStale: 48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy with no nodes, got %v", res.Reasons)
	}
}

func TestEvaluateStaleNodeIsUnhealthy(t *testing.T) {
	outputDir := t.TempDir()
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	tok, err := store.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := store.Enroll(tok, "pi")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(node.ID, 50, 100, ""); err != nil {
		t.Fatal(err)
	}

	later := time.Now().Add(72 * time.Hour)
	cfg := Config{
		OutputDir: outputDir, Roster: store, RunInterval: time.Hour, SyncGrace: time.Hour,
		SpaceCriticalPct: 90, NodeStale: 48 * time.Hour, Now: func() time.Time { return later },
	}
	res, err := Evaluate(cfg)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if res.Healthy {
		t.Fatal("expected unhealthy when an active node has been silent for 72h")
	}

	cfg.NodeStale = 0
	if res, err = Evaluate(cfg); err != nil || !res.Healthy {
		t.Fatalf("NodeStale=0 should disable the check, got %v %v", res.Reasons, err)
	}
}
