package stage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuiesceAndStageCopiesTree(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	writeFile(t, filepath.Join(src, "svc", "data.txt"), "payload")
	writeFile(t, filepath.Join(src, "top.txt"), "hi")

	staged, err := QuiesceAndStage(src, work, "testid", Options{Settle: time.Millisecond, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("QuiesceAndStage: %v", err)
	}

	if got := readFile(t, filepath.Join(staged, "svc", "data.txt")); got != "payload" {
		t.Errorf("staged content mismatch: %q", got)
	}
	if got := readFile(t, filepath.Join(staged, "top.txt")); got != "hi" {
		t.Errorf("staged content mismatch: %q", got)
	}
}

func TestQuiesceAndStageFailsWhenNoAttempts(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	writeFile(t, filepath.Join(src, "f.txt"), "x")

	if _, err := QuiesceAndStage(src, work, "id", Options{Settle: time.Millisecond, MaxAttempts: 0}); err == nil {
		t.Fatal("expected error when tree cannot be confirmed stable")
	}
}

func TestStatesEqual(t *testing.T) {
	a := fileState{"x": {size: 1, modNano: 2}}
	b := fileState{"x": {size: 1, modNano: 2}}
	if !statesEqual(a, b) {
		t.Fatal("expected equal states")
	}
	b["x"] = entry{size: 9, modNano: 2}
	if statesEqual(a, b) {
		t.Fatal("expected unequal states after size change")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
