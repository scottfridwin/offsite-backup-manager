package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestWriteTarZstHookAndTrailer(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "svc", "a.txt"), "hello")

	var buf bytes.Buffer
	stats, err := WriteTarZst(&buf, root, Options{
		Hook: func(rel string, hdr *tar.Header) {
			hdr.Uid, hdr.Gid = 1234, 5678
			if rel == "svc/a.txt" {
				hdr.Mode = 0o600
			}
		},
		Trailer: func(st Stats) (string, []byte, error) {
			if st.FileCount != 1 {
				t.Errorf("trailer saw file count %d, want 1", st.FileCount)
			}
			return ".meta.json", []byte(`{"ok":true}`), nil
		},
	})
	if err != nil {
		t.Fatalf("WriteTarZst: %v", err)
	}
	if stats.FileCount != 1 {
		t.Errorf("stats include the trailer: file count %d", stats.FileCount)
	}

	zr, err := zstd.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		switch hdr.Name {
		case "svc/a.txt":
			if hdr.Uid != 1234 || hdr.Gid != 5678 || hdr.Mode != 0o600 {
				t.Errorf("hook not applied: uid=%d gid=%d mode=%o", hdr.Uid, hdr.Gid, hdr.Mode)
			}
		case ".meta.json":
			b, _ := io.ReadAll(tr)
			if string(b) != `{"ok":true}` {
				t.Errorf("trailer content = %q", b)
			}
		}
	}
	if len(names) == 0 || names[len(names)-1] != ".meta.json" {
		t.Fatalf("trailer must be the last entry, got %v", names)
	}
}
