// Package archive writes a directory tree as a zstd-compressed tar stream.
package archive

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Entry summarizes one top-level directory/file of the archived tree.
type Entry struct {
	Name      string
	Bytes     int64
	FileCount int
}

// Stats reports what was written.
type Stats struct {
	FileCount  int
	TotalBytes int64
	topLevel   map[string]*Entry
}

// SortedEntries returns the top-level entries ordered by name (deterministic).
func (s Stats) SortedEntries() []Entry {
	out := make([]Entry, 0, len(s.topLevel))
	for _, e := range s.topLevel {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// WriteTarZst walks root and writes it as a zstd-compressed tar into dst. Only
// directories, regular files, and symlinks are archived; other special files
// are skipped. Sizes reported in Stats are the uncompressed file sizes.
func WriteTarZst(dst io.Writer, root string) (Stats, error) {
	st := Stats{topLevel: map[string]*Entry{}}

	zw, err := zstd.NewWriter(dst)
	if err != nil {
		return st, err
	}
	tw := tar.NewWriter(zw)

	root = filepath.Clean(root)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		isSymlink := info.Mode()&fs.ModeSymlink != 0
		if !info.IsDir() && !info.Mode().IsRegular() && !isSymlink {
			return nil // skip sockets, devices, pipes, etc.
		}

		var link string
		if isSymlink {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			n, err := io.Copy(tw, f)
			cerr := f.Close()
			if err != nil {
				return err
			}
			if cerr != nil {
				return cerr
			}
			st.FileCount++
			st.TotalBytes += n
			top := topLevel(rel)
			e := st.topLevel[top]
			if e == nil {
				e = &Entry{Name: top}
				st.topLevel[top] = e
			}
			e.Bytes += n
			e.FileCount++
		}
		return nil
	})
	if walkErr != nil {
		_ = tw.Close()
		_ = zw.Close()
		return st, walkErr
	}
	if err := tw.Close(); err != nil {
		_ = zw.Close()
		return st, err
	}
	if err := zw.Close(); err != nil {
		return st, err
	}
	return st, nil
}

func topLevel(rel string) string {
	rel = filepath.ToSlash(rel)
	if i := strings.Index(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return rel
}
