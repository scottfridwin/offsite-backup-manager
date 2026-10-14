package stage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestQuiesceAndStageRecordsSourceMetadata(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	file := filepath.Join(src, "svc", "secret.txt")
	writeFile(t, file, "s3cret")
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	// No owner-read: the staged copy must still be readable for archiving.
	if err := os.Chmod(file, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(file, 0o600) })
	if err := os.Chmod(filepath.Join(src, "svc"), 0o711); err != nil {
		t.Fatal(err)
	}

	if os.Getuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	// The source file is unreadable by its owner, so staging must fail loudly
	// rather than skip it.
	if _, err := QuiesceAndStage(src, work, "id", Options{Settle: time.Millisecond, MaxAttempts: 2}); err == nil {
		t.Fatal("expected an error for an unreadable source file")
	}

	if err := os.Chmod(file, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	res, err := QuiesceAndStage(src, work, "id2", Options{Settle: time.Millisecond, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("QuiesceAndStage: %v", err)
	}
	m, ok := res.Meta["svc/secret.txt"]
	if !ok {
		t.Fatalf("no metadata for svc/secret.txt: %v", res.Meta)
	}
	if m.Mode.Perm() != 0o400 || !m.ModTime.Equal(mtime) || m.UID != os.Getuid() {
		t.Fatalf("metadata = %+v, want mode 0400, mtime %s, uid %d", m, mtime, os.Getuid())
	}
	if d := res.Meta["svc"]; d.Mode.Perm() != 0o711 || !d.Mode.IsDir() {
		t.Fatalf("dir metadata = %+v, want 0711 dir", d)
	}
	if got := readFile(t, filepath.Join(res.Dir, "svc", "secret.txt")); got != "s3cret" {
		t.Fatalf("staged content = %q", got)
	}
}

func TestQuiesceAndStageReportsSpecialFiles(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "a")
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	res, err := QuiesceAndStage(src, work, "id", Options{Settle: time.Millisecond, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("QuiesceAndStage: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "pipe" {
		t.Fatalf("Skipped = %v, want [pipe]", res.Skipped)
	}
}

func TestQuiesceAndStageRetriesWhenSourceChangesDuringCopy(t *testing.T) {
	src := t.TempDir()
	work := t.TempDir()
	file := filepath.Join(src, "growing.log")
	writeFile(t, file, "x")

	// Keep appending until staging gives up: the source never holds still for
	// a whole copy, so no attempt may be accepted.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
			if err == nil {
				_, _ = f.WriteString("more")
				_ = f.Close()
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_, err := QuiesceAndStage(src, work, "id", Options{Settle: 5 * time.Millisecond, MaxAttempts: 3})
	close(stop)
	<-done
	if err == nil {
		t.Fatal("expected staging to fail while the source keeps changing")
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Fatalf("staging dir left behind: %v", entries)
	}
}
