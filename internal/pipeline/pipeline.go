// Package pipeline wires the Primary-side packaging steps together:
// quiesce + stage -> tar -> zstd -> age-encrypt -> signed manifest.
package pipeline

import (
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
	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
	"github.com/scottfridwin/offsite-backup-manager/internal/stage"
)

// RunIDFormat is the UTC timestamp layout used for run identifiers.
const RunIDFormat = "20060102T150405Z"

// Result reports where the run's artifacts were written.
type Result struct {
	RunID         string
	PackagePath   string
	ManifestPath  string
	SignaturePath string
	Manifest      manifest.Manifest
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
			return nil, errors.New("another packaging run is in progress")
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
// detached signature into the output directory.
func Run(cfg config.Config, version string) (Result, error) {
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

	runID := time.Now().UTC().Format(RunIDFormat)
	pkgName := "run-" + runID + ".tar.zst.age"
	pkgPath := filepath.Join(cfg.OutputDir, pkgName)
	if _, err := os.Stat(pkgPath); err == nil {
		return Result{}, fmt.Errorf("run %s already exists; retry in a second", runID)
	}

	stagedDir, err := stage.QuiesceAndStage(cfg.SourceDir, cfg.WorkDir, runID, stage.DefaultOptions())
	if err != nil {
		return Result{}, fmt.Errorf("stage: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagedDir) }()

	stats, size, sum, err := writePackage(pkgPath, stagedDir, recipients)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			_ = os.Remove(pkgPath)
		}
		return Result{}, fmt.Errorf("package: %w", err)
	}

	m := manifest.New(runID)
	m.SourceDir = cfg.SourceDir
	m.Package = manifest.Package{
		Name:        pkgName,
		Bytes:       size,
		SHA256:      sum,
		Compression: "zstd",
		Encryption:  "age",
	}
	m.Contents = buildContents(stats)
	m.Distribution, err = buildDistribution(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("distribution assignment: %w", err)
	}
	m.Tool = manifest.Tool{Name: "offsite-backup-manager", Version: version}

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

	return Result{
		RunID:         runID,
		PackagePath:   pkgPath,
		ManifestPath:  manifestPath,
		SignaturePath: sigPath,
		Manifest:      m,
	}, nil
}

// writePackage streams the staged tree through tar -> zstd -> age into pkgPath,
// computing the ciphertext size and SHA-256 as it goes.
func writePackage(pkgPath, stagedDir string, recipients []age.Recipient) (archive.Stats, int64, string, error) {
	// O_EXCL: a published run is never overwritten.
	f, err := os.OpenFile(pkgPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return archive.Stats{}, 0, "", err
	}

	h := sha256.New()
	cw := &countWriter{}
	dst := io.MultiWriter(f, h, cw)

	encW, err := cryptoutil.EncryptWriter(dst, recipients)
	if err != nil {
		_ = f.Close()
		return archive.Stats{}, 0, "", err
	}

	stats, archiveErr := archive.WriteTarZst(encW, stagedDir)
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

	return stats, cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

func buildContents(stats archive.Stats) manifest.Contents {
	c := manifest.Contents{
		FileCount:  stats.FileCount,
		TotalBytes: stats.TotalBytes,
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
