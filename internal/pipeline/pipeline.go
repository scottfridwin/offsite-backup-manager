// Package pipeline wires the Primary-side packaging steps together:
// quiesce + stage -> tar -> zstd -> age-encrypt -> signed manifest.
package pipeline

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"filippo.io/age"
	"golang.org/x/sys/unix"

	"github.com/scottfridwin/offsite-backup-manager/internal/archive"
	"github.com/scottfridwin/offsite-backup-manager/internal/config"
	"github.com/scottfridwin/offsite-backup-manager/internal/cryptoutil"
	"github.com/scottfridwin/offsite-backup-manager/internal/health"
	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
	"github.com/scottfridwin/offsite-backup-manager/internal/stage"
)

// RunIDFormat is the UTC timestamp layout used for run identifiers.
const RunIDFormat = "20060102T150405Z"

// EmbeddedManifestName is the manifest written as the last entry of every
// package's tar, so a decrypted package describes itself even without its
// detached manifest. It has no package checksum (not known until encrypted).
const EmbeddedManifestName = ".offsite-backup-manifest.json"

// ErrBusy means another packaging run holds the output directory lock.
var ErrBusy = errors.New("another packaging run is in progress")

// Result reports where the run's artifacts were written.
type Result struct {
	RunID         string
	PackagePath   string
	ManifestPath  string
	SignaturePath string
	Manifest      manifest.Manifest
	// Pruned lists run IDs removed from the output directory (PACKAGE_KEEP_RUNS).
	Pruned []string
}

type countWriter struct{ n int64 }

// lockOutputDir takes an exclusive, non-blocking lock on outputDir and
// returns its release function.
func lockOutputDir(outputDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(outputDir, ".package.lock"), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock output dir: %w", err)
	}
	return func() { _ = f.Close() }, nil
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// Run executes one packaging run and writes the package, manifest, and
// detached signature into the output directory. Its outcome is recorded for
// the healthcheck (a run that could not start because another holds the lock
// is not an outcome).
func Run(cfg config.Config, version string) (Result, error) {
	res, err := run(cfg, version)
	if !errors.Is(err, ErrBusy) && cfg.OutputDir != "" {
		_ = health.WriteRunStatus(cfg.OutputDir, health.RunStatus{
			FinishedAt: time.Now().UTC(),
			RunID:      res.RunID,
			Error:      errString(err),
		})
	}
	return res, err
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func run(cfg config.Config, version string) (Result, error) {
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}

	recipients, err := cryptoutil.ParseRecipients(cfg.AgeRecipients)
	if err != nil {
		return Result{}, err
	}
	signer, err := cryptoutil.LoadSigner(cfg.MinisignKeyFile, cfg.MinisignPassword)
	if err != nil {
		return Result{}, err
	}

	if err := os.MkdirAll(cfg.OutputDir, 0o750); err != nil {
		return Result{}, err
	}
	// The scheduler and a manual `package` can overlap; their run IDs (one
	// second resolution) and staging dirs would collide, so allow one at a time.
	unlock, err := lockOutputDir(cfg.OutputDir)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	removeLeftovers(cfg.OutputDir, cfg.WorkDir)

	runID := time.Now().UTC().Format(RunIDFormat)
	pkgName := "run-" + runID + ".tar.zst.age"
	pkgPath := filepath.Join(cfg.OutputDir, pkgName)
	if _, err := os.Stat(pkgPath); err == nil {
		return Result{}, fmt.Errorf("run %s already exists; retry in a second", runID)
	}

	staged, err := stage.QuiesceAndStage(cfg.SourceDir, cfg.WorkDir, runID, stage.DefaultOptions())
	if err != nil {
		return Result{}, fmt.Errorf("stage: %w", err)
	}
	defer func() { _ = os.RemoveAll(staged.Dir) }()

	m := manifest.New(runID)
	m.SourceDir = cfg.SourceDir
	m.Tool = manifest.Tool{Name: "offsite-backup-manager", Version: version}
	m.Package = manifest.Package{Name: pkgName, Compression: "zstd", Encryption: "age"}
	m.Distribution, err = buildDistribution(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("distribution assignment: %w", err)
	}

	opts := archive.Options{
		Hook: sourceMetadata(staged.Meta),
		Trailer: func(stats archive.Stats) (string, []byte, error) {
			embedded := m
			embedded.Contents = buildContents(stats, staged.Skipped)
			b, err := embedded.Marshal()
			return EmbeddedManifestName, b, err
		},
	}
	stats, size, sum, err := writePackage(pkgPath, staged.Dir, recipients, opts)
	if err != nil {
		return Result{}, fmt.Errorf("package: %w", err)
	}

	m.Package.Bytes = size
	m.Package.SHA256 = sum
	m.Contents = buildContents(stats, staged.Skipped)

	mb, err := m.Marshal()
	if err != nil {
		return Result{}, err
	}
	manifestPath := filepath.Join(cfg.OutputDir, "run-"+runID+".manifest.json")
	if err := os.WriteFile(manifestPath, mb, 0o640); err != nil {
		return Result{}, err
	}

	sigPath := manifestPath + ".minisig"
	if err := os.WriteFile(sigPath, signer.Sign(mb), 0o640); err != nil {
		return Result{}, err
	}

	res := Result{
		RunID:         runID,
		PackagePath:   pkgPath,
		ManifestPath:  manifestPath,
		SignaturePath: sigPath,
		Manifest:      m,
	}
	if cfg.KeepRuns > 0 {
		res.Pruned, err = prune(cfg.OutputDir, cfg.NodeRosterFile, cfg.KeepRuns)
		if err != nil {
			return res, fmt.Errorf("run %s published, but pruning older runs failed: %w", runID, err)
		}
	}
	return res, nil
}

// removeLeftovers deletes what a run interrupted by a crash or restart left
// behind. Callers must hold the output directory lock.
func removeLeftovers(outputDir, workDir string) {
	for _, pattern := range []string{
		filepath.Join(outputDir, ".run-*.partial"),
		filepath.Join(workDir, "stage-*"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, p := range matches {
			_ = os.RemoveAll(p)
		}
	}
}

// sourceMetadata returns a header hook restoring each entry's source owner,
// mode and mtime (the staged copy belongs to the packaging user). Names are
// cleared so a restore maps numeric IDs, not whatever names the restore host
// happens to have.
func sourceMetadata(meta map[string]stage.Meta) archive.HeaderHook {
	return func(rel string, hdr *tar.Header) {
		m, ok := meta[rel]
		if !ok {
			return
		}
		if m.UID >= 0 {
			hdr.Uid, hdr.Gid = m.UID, m.GID
		}
		hdr.Uname, hdr.Gname = "", ""
		hdr.Mode = int64(m.Mode.Perm())
		if m.Mode&fs.ModeSetuid != 0 {
			hdr.Mode |= 0o4000
		}
		if m.Mode&fs.ModeSetgid != 0 {
			hdr.Mode |= 0o2000
		}
		if m.Mode&fs.ModeSticky != 0 {
			hdr.Mode |= 0o1000
		}
		hdr.ModTime = m.ModTime
	}
}

// writePackage streams the staged tree through tar -> zstd -> age into a
// temporary file, computing the ciphertext size and SHA-256 as it goes, then
// publishes it as pkgPath. A published run is never overwritten.
func writePackage(pkgPath, stagedDir string, recipients []age.Recipient, opts archive.Options) (archive.Stats, int64, string, error) {
	tmpPath := filepath.Join(filepath.Dir(pkgPath), "."+filepath.Base(pkgPath)+".partial")
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return archive.Stats{}, 0, "", err
	}
	defer func() { _ = os.Remove(tmpPath) }()

	h := sha256.New()
	cw := &countWriter{}
	dst := io.MultiWriter(f, h, cw)

	encW, err := cryptoutil.EncryptWriter(dst, recipients)
	if err != nil {
		_ = f.Close()
		return archive.Stats{}, 0, "", err
	}

	stats, archiveErr := archive.WriteTarZst(encW, stagedDir, opts)
	// Close the age writer to flush the final ciphertext before hashing/sizing.
	closeErr := encW.Close()
	syncErr := f.Sync()
	fcloseErr := f.Close()

	switch {
	case archiveErr != nil:
		return stats, 0, "", archiveErr
	case closeErr != nil:
		return stats, 0, "", closeErr
	case syncErr != nil:
		return stats, 0, "", syncErr
	case fcloseErr != nil:
		return stats, 0, "", fcloseErr
	}

	// Link (unlike rename) fails if pkgPath already exists.
	if err := os.Link(tmpPath, pkgPath); err != nil {
		return stats, 0, "", err
	}
	return stats, cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

func buildContents(stats archive.Stats, skipped []string) manifest.Contents {
	c := manifest.Contents{
		FileCount:  stats.FileCount,
		TotalBytes: stats.TotalBytes,
		Skipped:    skipped,
	}
	for _, e := range stats.SortedEntries() {
		c.TopLevel = append(c.TopLevel, manifest.EntrySummary{
			Name:      e.Name,
			Bytes:     e.Bytes,
			FileCount: e.FileCount,
		})
	}
	return c
}

// buildDistribution assigns this run to node(s) per the configured scheme
// (§8). Replicate-all is resolved at pull time by the server (every
// currently-active node is eligible); round-robin is resolved here, once,
// against the roster's rotation cursor, and recorded so it never changes.
func buildDistribution(cfg config.Config) (manifest.Distribution, error) {
	scheme := cfg.DistributionScheme
	if scheme == "" {
		scheme = manifest.SchemeReplicateAll
	}
	if scheme != manifest.SchemeRoundRobin {
		return manifest.Distribution{Scheme: scheme}, nil
	}

	node, err := roster.Open(cfg.NodeRosterFile).NextRoundRobin()
	if err != nil {
		return manifest.Distribution{}, err
	}
	return manifest.Distribution{Scheme: scheme, AssignedNodeIDs: []string{node.ID}}, nil
}
