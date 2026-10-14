package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

func publishFakeRun(t *testing.T, outputDir, id string) {
	t.Helper()
	m := manifest.New(id)
	m.Distribution = manifest.Distribution{Scheme: manifest.SchemeReplicateAll}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(outputDir, "run-"+id)
	writeFile(t, base+".tar.zst.age", "pkg")
	if err := os.WriteFile(base+".manifest.json", b, 0o640); err != nil {
		t.Fatal(err)
	}
	writeFile(t, base+".manifest.json.minisig", "sig")
}

func TestPruneKeepsUndeliveredRuns(t *testing.T) {
	out := t.TempDir()
	rosterPath := filepath.Join(t.TempDir(), "roster.json")
	ids := []string{"20260101T000000Z", "20260201T000000Z", "20260301T000000Z"}
	for _, id := range ids {
		publishFakeRun(t, out, id)
	}

	s := roster.Open(rosterPath)
	node := enrollNode(t, s, "pi")
	if err := s.Heartbeat(node.ID, 1, 2, ids[0]); err != nil {
		t.Fatal(err)
	}

	// The node has only confirmed the first run: the second must stay even
	// though it's beyond keep=1.
	pruned, err := prune(out, rosterPath, 1)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != ids[0] {
		t.Fatalf("pruned %v, want [%s]", pruned, ids[0])
	}
	left, _ := publishedRuns(out)
	if len(left) != 2 || left[0] != ids[1] {
		t.Fatalf("remaining runs %v", left)
	}
	for _, suffix := range []string{".tar.zst.age", ".manifest.json", ".manifest.json.minisig"} {
		if _, err := os.Stat(filepath.Join(out, "run-"+ids[0]+suffix)); !os.IsNotExist(err) {
			t.Errorf("run-%s%s still present", ids[0], suffix)
		}
	}

	if err := s.Heartbeat(node.ID, 1, 2, ids[2]); err != nil {
		t.Fatal(err)
	}
	if pruned, err = prune(out, rosterPath, 1); err != nil || len(pruned) != 1 || pruned[0] != ids[1] {
		t.Fatalf("second prune = %v, %v; want [%s]", pruned, err, ids[1])
	}
}

func TestPruneWithoutNodesKeepsNewest(t *testing.T) {
	out := t.TempDir()
	for _, id := range []string{"20260101T000000Z", "20260201T000000Z", "20260301T000000Z"} {
		publishFakeRun(t, out, id)
	}
	pruned, err := prune(out, filepath.Join(t.TempDir(), "roster.json"), 2)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(pruned) != 1 {
		t.Fatalf("pruned %v, want one run", pruned)
	}
	if left, _ := publishedRuns(out); len(left) != 2 {
		t.Fatalf("remaining runs %v, want 2", left)
	}
}

func TestRemoveLeftovers(t *testing.T) {
	out := t.TempDir()
	work := t.TempDir()
	writeFile(t, filepath.Join(out, ".run-20260101T000000Z.tar.zst.age.partial"), "half")
	writeFile(t, filepath.Join(work, "stage-20260101T000000Z", "f"), "x")
	writeFile(t, filepath.Join(work, "unrelated"), "keep")
	publishFakeRun(t, out, "20260201T000000Z")

	removeLeftovers(out, work)

	if m, _ := filepath.Glob(filepath.Join(out, ".*.partial")); len(m) != 0 {
		t.Errorf("partial package not removed: %v", m)
	}
	if _, err := os.Stat(filepath.Join(work, "stage-20260101T000000Z")); !os.IsNotExist(err) {
		t.Errorf("stale staging dir not removed")
	}
	if _, err := os.Stat(filepath.Join(work, "unrelated")); err != nil {
		t.Errorf("unrelated file removed: %v", err)
	}
	if left, _ := publishedRuns(out); len(left) != 1 {
		t.Errorf("published run removed")
	}
}
