package capacity

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

func writeRun(t *testing.T, outputDir, runID string, createdAt time.Time, bytes int64) {
	t.Helper()
	m := manifest.New(runID)
	m.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	m.Package = manifest.Package{Name: "run-" + runID + ".tar.zst.age", Bytes: bytes, SHA256: "abc"}
	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	base := "run-" + runID
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

func TestComputeNoRuns(t *testing.T) {
	proj, err := Compute(t.TempDir())
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if proj.RunCount != 0 || proj.HasCadence {
		t.Fatalf("expected empty projection, got %+v", proj)
	}
}

func TestComputeSingleRun(t *testing.T) {
	outputDir := t.TempDir()
	writeRun(t, outputDir, "20260101T000000Z", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 1000)

	proj, err := Compute(outputDir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if proj.RunCount != 1 || proj.AvgPackageBytes != 1000 {
		t.Fatalf("unexpected projection: %+v", proj)
	}
	if proj.HasCadence {
		t.Fatalf("expected no cadence estimate from a single run: %+v", proj)
	}
}

func TestComputeMultipleRuns(t *testing.T) {
	outputDir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeRun(t, outputDir, "20260101T000000Z", base, 1000)
	writeRun(t, outputDir, "20260108T000000Z", base.Add(7*24*time.Hour), 2000)
	writeRun(t, outputDir, "20260115T000000Z", base.Add(14*24*time.Hour), 3000)

	proj, err := Compute(outputDir)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if proj.RunCount != 3 {
		t.Fatalf("run count = %d, want 3", proj.RunCount)
	}
	if proj.AvgPackageBytes != 2000 {
		t.Fatalf("avg package bytes = %d, want 2000", proj.AvgPackageBytes)
	}
	if !proj.HasCadence || proj.AvgCadence != 7*24*time.Hour {
		t.Fatalf("avg cadence = %v (hasCadence=%v), want 168h", proj.AvgCadence, proj.HasCadence)
	}
	wantCadence := 7 * 24 * time.Hour
	wantGrowth := int64(float64(2000) * (float64(YearDuration) / float64(wantCadence)))
	if proj.GrowthPerYr != wantGrowth {
		t.Fatalf("growth/yr = %d, want %d", proj.GrowthPerYr, wantGrowth)
	}
}

func TestNodeProjections(t *testing.T) {
	proj := Projection{AvgPackageBytes: 1000, AvgCadence: 7 * 24 * time.Hour, HasCadence: true}
	nodes := []roster.Node{
		{ID: "a", Label: "pi-a", FreeBytes: 5000},
		{ID: "b", Label: "pi-b", FreeBytes: 0}, // no heartbeat yet
	}

	got := NodeProjections(proj, nodes)
	if len(got) != 2 {
		t.Fatalf("expected 2 projections, got %d", len(got))
	}
	if !got[0].HasEstimate || got[0].TimeToFull != 5*7*24*time.Hour {
		t.Fatalf("node a projection = %+v, want time-to-full 35 days", got[0])
	}
	if got[1].HasEstimate {
		t.Fatalf("node b should have no estimate without free space telemetry: %+v", got[1])
	}
}

func TestNodeProjectionsNoCadence(t *testing.T) {
	proj := Projection{AvgPackageBytes: 1000, HasCadence: false}
	got := NodeProjections(proj, []roster.Node{{ID: "a", FreeBytes: 5000}})
	if got[0].HasEstimate {
		t.Fatalf("expected no estimate without cadence history: %+v", got[0])
	}
}
