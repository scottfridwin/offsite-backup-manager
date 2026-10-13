// Package stage produces a stable, point-in-time copy of the backup source
// tree before it is packaged, so a run never captures a half-written file.
package stage

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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

// Meta is an entry's metadata as found in the source tree. The staged copy is
// owned by the (unprivileged) packaging user, so the archive takes ownership,
// mode and mtime from here instead.
type Meta struct {
	UID, GID int
	Mode     fs.FileMode
	ModTime  time.Time
}

// Result is a completed staging copy.
type Result struct {
	Dir  string
	Meta map[string]Meta // keyed by slash-separated path relative to the root
	// Skipped lists special files (sockets, devices, pipes) left out of the copy.
	Skipped []string
}

// QuiesceAndStage waits for root to stop changing, copies it to a fresh
// staging directory under workDir, then re-scans root: if anything changed
// during the copy, the copy is discarded and the cycle repeats. The caller is
// responsible for removing Result.Dir when done.
func QuiesceAndStage(root, workDir, id string, opt Options) (Result, error) {
	root = filepath.Clean(root)
	dst := filepath.Join(workDir, "stage-"+id)
	var skipped []string

	prev, err := scan(root)
	if err != nil {
		return Result{}, err
	}
	for i := 0; i < opt.MaxAttempts; i++ {
		time.Sleep(opt.Settle)
		cur, err := scan(root)
		if err != nil {
			return Result{}, err
		}
		if !statesEqual(prev, cur) {
			prev = cur
			continue
		}

		meta, err := copyTree(root, dst, &skipped)
		if err != nil {
			_ = os.RemoveAll(dst)
			return Result{}, err
		}
		after, err := scan(root)
		if err != nil {
			_ = os.RemoveAll(dst)
			return Result{}, err
		}
		if statesEqual(cur, after) {
			return Result{Dir: dst, Meta: meta, Skipped: skipped}, nil
		}
		// A file changed while it was being copied; the copy may hold a
		// half-written file.
		if err := os.RemoveAll(dst); err != nil {
			return Result{}, err
		}
		prev = after
		skipped = nil
	}
	return Result{}, fmt.Errorf("source %q did not quiesce after %d attempts", root, opt.MaxAttempts)
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

func copyTree(src, dst string, skipped *[]string) (map[string]Meta, error) {
	src = filepath.Clean(src)
	meta := map[string]Meta{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
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
		m := Meta{UID: -1, GID: -1, Mode: info.Mode(), ModTime: info.ModTime()}
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			m.UID, m.GID = int(sys.Uid), int(sys.Gid)
		}
		meta[filepath.ToSlash(rel)] = m

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
			if err := copyFile(path, target); err != nil {
				return err
			}
			return os.Chtimes(target, info.ModTime(), info.ModTime())
		default:
			*skipped = append(*skipped, filepath.ToSlash(rel))
			return nil // sockets, devices, pipes: nothing to back up
		}
	})
	return meta, err
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	// 0600, not the source mode: a source file without owner-read must still be
	// readable when archived; its real mode is in Meta.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
