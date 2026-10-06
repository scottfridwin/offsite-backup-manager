package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestWriteTarZst(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "svcA", "a.txt"), "hello")
	mustWrite(t, filepath.Join(root, "svcA", "b.txt"), "world!!")
	mustWrite(t, filepath.Join(root, "svcB", "c.bin"), "xyz")

	var buf bytes.Buffer
	stats, err := WriteTarZst(&buf, root)
	if err != nil {
		t.Fatalf("WriteTarZst: %v", err)
	}
	if stats.FileCount != 3 {
		t.Errorf("file count = %d, want 3", stats.FileCount)
	}
	if stats.TotalBytes != int64(len("hello")+len("world!!")+len("xyz")) {
		t.Errorf("total bytes = %d", stats.TotalBytes)
	}

	entries := stats.SortedEntries()
	if len(entries) != 2 || entries[0].Name != "svcA" || entries[1].Name != "svcB" {
		t.Fatalf("unexpected top-level entries: %+v", entries)
	}
	if entries[0].FileCount != 2 || entries[1].FileCount != 1 {
		t.Errorf("unexpected per-entry file counts: %+v", entries)
	}

	got := readTarZst(t, &buf)
	for _, name := range []string{"svcA/a.txt", "svcA/b.txt", "svcB/c.bin"} {
		if _, ok := got[name]; !ok {
			t.Errorf("missing %q in archive; have %v", name, keys(got))
		}
	}
	if got["svcA/a.txt"] != "hello" {
		t.Errorf("content mismatch for svcA/a.txt: %q", got["svcA/a.txt"])
	}
}

func readTarZst(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	zr, err := zstd.NewReader(r)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %q: %v", hdr.Name, err)
			}
			out[hdr.Name] = string(b)
		}
	}
	return out
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
