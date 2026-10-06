// Package stage produces a stable, point-in-time copy of the backup source
// tree before it is packaged, so a run never captures a half-written file.
package stage

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Options tunes the quiescence check.
type Options struct {
	// Settle is how long to wait between stability scans.
	Settle time.Duration
	// MaxAttempts is how many settle+rescan cycles to allow before giving up.
	MaxAttempts int
}

// DefaultOptions returns production-sane defaults.
func DefaultOptions() Options {
	return Options{Settle: 2 * time.Second, MaxAttempts: 5}
}

// entry records the size and mtime of a regular file, used to detect writes.
type entry struct {
	size    int64
	modNano int64
}

type fileState map[string]entry

// QuiesceAndStage waits for root to stop changing, then copies it to a fresh
// staging directory under workDir and returns that path. The caller is
// responsible for removing the staging directory when done.
func QuiesceAndStage(root, workDir, id string, opt Options) (string, error) {
	root = filepath.Clean(root)

	prev, err := scan(root)
	if err != nil {
		return "", err
	}
	stable := false
	for i := 0; i < opt.MaxAttempts; i++ {
		time.Sleep(opt.Settle)
		cur, err := scan(root)
		if err != nil {
			return "", err
		}
		if statesEqual(prev, cur) {
			stable = true
			break
		}
		prev = cur
	}
	if !stable {
		return "", fmt.Errorf("source %q did not quiesce after %d attempts", root, opt.MaxAttempts)
	}

	dst := filepath.Join(workDir, "stage-"+id)
	if err := copyTree(root, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func scan(root string) (fileState, error) {
	state := fileState{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			state[rel] = entry{size: info.Size(), modNano: info.ModTime().UnixNano()}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

func statesEqual(a, b fileState) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va != vb {
			return false
		}
	}
	return true
}

func copyTree(src, dst string) error {
	src = filepath.Clean(src)
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o750)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			if err := copyFile(path, target, info); err != nil {
				return err
			}
			return os.Chtimes(target, info.ModTime(), info.ModTime())
		default:
			return nil // skip special files
		}
	})
}

func copyFile(src, dst string, info fs.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
