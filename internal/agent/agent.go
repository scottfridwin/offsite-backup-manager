// Package agent implements the backup-node pull client: one-time enrollment,
// periodic append-only pulling of new runs (verifying signature + checksum
// before ever touching the store), and heartbeat reporting. It never deletes
// or overwrites anything it has already stored.
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/scottfridwin/offsite-backup-manager/internal/cryptoutil"
	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
)

// Config configures a node agent.
type Config struct {
	// BackupHost is the Primary's public endpoint, e.g. "backup.example.com"
	// (BACKUP_HOST). Always addressed over HTTPS.
	BackupHost string
	// EnrollmentToken is the one-time bootstrap secret, consumed on first
	// enrollment only (ENROLLMENT_TOKEN).
	EnrollmentToken string
	// NodeLabel is this node's human-friendly name (NODE_LABEL).
	NodeLabel string
	// PullInterval is how often the agent polls for new runs (PULL_INTERVAL).
	PullInterval time.Duration
	// StoreDir is the append-only package store (STORE_DIR).
	StoreDir string
	// MinisignPubKeyFile verifies the manifest signature published by the Primary.
	MinisignPubKeyFile string
	// CapacityWarnPct is the local free-space warning threshold, 0-100 (CAPACITY_WARN_PCT).
	CapacityWarnPct float64
	// DownloadIdleTimeout aborts a package download only if no bytes arrive for
	// this long (DOWNLOAD_IDLE_TIMEOUT). Unlike a whole-request timeout it never
	// caps the total transfer time, so multi-GB packages can download over slow
	// links as long as they keep making progress. Defaults to 2 minutes.
	DownloadIdleTimeout time.Duration

	// HTTPClient overrides the default HTTP client (tests only).
	HTTPClient *http.Client
	// Logger receives operational diagnostics. Defaults to slog.Default().
	Logger *slog.Logger
}

// credentialFileName is the durable enrollment credential written into
// StoreDir once enrollment succeeds.
const credentialFileName = ".node-credential.json"

type credential struct {
	NodeID    string `json:"node_id"`
	PullToken string `json:"pull_token"`
}

// Agent is a configured node pull client.
type Agent struct {
	cfg      Config
	client   *http.Client
	dlClient *http.Client
	dlIdle   time.Duration
	verifier *cryptoutil.Verifier
	baseURL  string
}

// New validates cfg and returns a ready-to-use Agent.
func New(cfg Config) (*Agent, error) {
	if cfg.BackupHost == "" {
		return nil, fmt.Errorf("backup host is required (set BACKUP_HOST)")
	}
	if cfg.StoreDir == "" {
		return nil, fmt.Errorf("store directory is required (set STORE_DIR)")
	}
	if cfg.MinisignPubKeyFile == "" {
		return nil, fmt.Errorf("a minisign public key is required (set MINISIGN_PUBKEY)")
	}
	if cfg.PullInterval <= 0 {
		cfg.PullInterval = time.Hour
	}
	if cfg.CapacityWarnPct <= 0 {
		cfg.CapacityWarnPct = 90
	}
	if cfg.DownloadIdleTimeout <= 0 {
		cfg.DownloadIdleTimeout = 2 * time.Minute
	}
	// The package download must not be bound by a whole-request timeout: an
	// 8-20 GB package over a slow offsite link legitimately takes far longer
	// than any fixed deadline. Small control requests keep a short overall
	// timeout; the download instead relies on the idle/stall timeout below.
	dlClient := cfg.HTTPClient
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
		dlClient = &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 2 * time.Minute,
				ExpectContinueTimeout: 1 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	verifier, err := cryptoutil.LoadVerifier(cfg.MinisignPubKeyFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StoreDir, 0o750); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}

	return &Agent{
		cfg:      cfg,
		client:   cfg.HTTPClient,
		dlClient: dlClient,
		dlIdle:   cfg.DownloadIdleTimeout,
		verifier: verifier,
		baseURL:  "https://" + cfg.BackupHost,
	}, nil
}

func (a *Agent) credentialPath() string {
	return filepath.Join(a.cfg.StoreDir, credentialFileName)
}

// EnsureEnrolled returns the node's durable pull credential, enrolling with
// the one-time bootstrap token on first run and persisting the result.
func (a *Agent) EnsureEnrolled(ctx context.Context) (string, error) {
	if b, err := os.ReadFile(a.credentialPath()); err == nil {
		var cred credential
		if err := json.Unmarshal(b, &cred); err == nil && cred.PullToken != "" {
			return cred.PullToken, nil
		}
	}

	if a.cfg.EnrollmentToken == "" {
		return "", fmt.Errorf("no stored credential and no ENROLLMENT_TOKEN provided")
	}

	reqBody, err := json.Marshal(map[string]string{
		"token": a.cfg.EnrollmentToken,
		"label": a.cfg.NodeLabel,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/enroll", bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("enroll request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("enroll failed: %s: %s", resp.Status, string(b))
	}

	var out struct {
		NodeID    string `json:"node_id"`
		PullToken string `json:"pull_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode enroll response: %w", err)
	}

	cred := credential{NodeID: out.NodeID, PullToken: out.PullToken}
	credBytes, err := json.Marshal(cred)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(a.credentialPath(), credBytes, 0o600); err != nil {
		return "", fmt.Errorf("persist credential: %w", err)
	}
	return cred.PullToken, nil
}

type runSummary struct {
	RunID        string `json:"run_id"`
	ManifestURL  string `json:"manifest_url"`
	SignatureURL string `json:"signature_url"`
	PackageURL   string `json:"package_url"`
}

// RunOnce fetches the list of runs the Primary knows about and pulls any that
// are not already in the store, verifying each before it is kept. It returns
// the most recent run ID successfully synced (possibly already-synced), for
// use in the next heartbeat.
func (a *Agent) RunOnce(ctx context.Context, pullToken string) (string, error) {
	runs, err := a.fetchRuns(ctx, pullToken)
	if err != nil {
		return "", err
	}

	lastSynced := ""
	var firstErr error
	for _, run := range runs {
		pkgPath := filepath.Join(a.cfg.StoreDir, "run-"+run.RunID+".tar.zst.age")
		if fileExists(pkgPath) {
			lastSynced = run.RunID
			continue
		}
		if err := a.pullRun(ctx, pullToken, run); err != nil {
			a.cfg.Logger.Error("pull run failed", "run_id", run.RunID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		lastSynced = run.RunID
	}
	return lastSynced, firstErr
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (a *Agent) fetchRuns(ctx context.Context, pullToken string) ([]runSummary, error) {
	resp, err := a.authGet(ctx, "/runs", pullToken)
	if err != nil {
		return nil, fmt.Errorf("fetch runs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch runs: %s: %s", resp.Status, string(b))
	}
	var out struct {
		Runs []runSummary `json:"runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode runs response: %w", err)
	}
	return out.Runs, nil
}

// pullRun downloads, verifies, and stores a single run's artifacts. It only
// ever creates new files (O_EXCL) and marks them read-only, approximating
// WORM at the filesystem layer; true immutability additionally requires a
// deployment-level control such as `chattr +a` on the store directory.
func (a *Agent) pullRun(ctx context.Context, pullToken string, run runSummary) error {
	manifestBytes, err := a.fetchBytes(ctx, pullToken, run.ManifestURL)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}
	sigBytes, err := a.fetchBytes(ctx, pullToken, run.SignatureURL)
	if err != nil {
		return fmt.Errorf("fetch signature: %w", err)
	}
	if !a.verifier.Verify(manifestBytes, sigBytes) {
		return fmt.Errorf("manifest signature verification failed for run %s", run.RunID)
	}

	var m manifest.Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if m.RunID != run.RunID {
		return fmt.Errorf("manifest run_id %q does not match advertised run %q", m.RunID, run.RunID)
	}

	pkgTmp, pkgSum, pkgSize, err := a.downloadToTemp(ctx, pullToken, run.PackageURL)
	if err != nil {
		return fmt.Errorf("fetch package: %w", err)
	}
	defer func() { _ = os.Remove(pkgTmp) }()

	if pkgSum != m.Package.SHA256 {
		return fmt.Errorf("package checksum mismatch for run %s: got %s want %s", run.RunID, pkgSum, m.Package.SHA256)
	}
	if m.Package.Bytes != 0 && pkgSize != m.Package.Bytes {
		return fmt.Errorf("package size mismatch for run %s: got %d want %d", run.RunID, pkgSize, m.Package.Bytes)
	}

	base := "run-" + run.RunID
	if err := a.commitFile(pkgTmp, base+".tar.zst.age"); err != nil {
		return err
	}
	if err := a.writeOnce(base+".manifest.json", manifestBytes); err != nil {
		return err
	}
	if err := a.writeOnce(base+".manifest.json.minisig", sigBytes); err != nil {
		return err
	}
	return nil
}

// writeOnce creates path (relative to StoreDir) exclusively and makes it
// read-only, refusing to touch an existing file.
func (a *Agent) writeOnce(relName string, data []byte) error {
	path := filepath.Join(a.cfg.StoreDir, relName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil // already stored; never overwrite
		}
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(path)
		return werr
	}
	if cerr != nil {
		return cerr
	}
	return os.Chmod(path, 0o400)
}

// commitFile moves a verified temp file into the store exclusively and marks
// it read-only.
func (a *Agent) commitFile(tmpPath, relName string) error {
	path := filepath.Join(a.cfg.StoreDir, relName)
	if fileExists(path) {
		return nil // already stored; never overwrite
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o400)
}

func (a *Agent) authGet(ctx context.Context, path, pullToken string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pullToken)
	return a.client.Do(req)
}

func (a *Agent) fetchBytes(ctx context.Context, pullToken, path string) ([]byte, error) {
	resp, err := a.authGet(ctx, path, pullToken)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s: %s: %s", path, resp.Status, string(b))
	}
	return io.ReadAll(resp.Body)
}

// downloadToTemp streams path into a temp file under StoreDir, returning its
// path, hex sha256, and byte count. The transfer is bounded only by an
// idle/stall timeout (no whole-request deadline), so arbitrarily large
// packages download successfully as long as bytes keep arriving.
func (a *Agent) downloadToTemp(ctx context.Context, pullToken, path string) (string, string, int64, error) {
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, a.baseURL+path, nil)
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+pullToken)

	resp, err := a.dlClient.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", "", 0, fmt.Errorf("%s: %s: %s", path, resp.Status, string(b))
	}

	tmp, err := os.CreateTemp(a.cfg.StoreDir, ".download-*.tmp")
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = tmp.Close() }()

	// Cancel the request if the body stalls (no bytes) for dlIdle; each
	// successful read resets the watchdog.
	idle := time.AfterFunc(a.dlIdle, cancel)
	defer idle.Stop()
	body := &stallReader{r: resp.Body, reset: func() { idle.Reset(a.dlIdle) }}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err != nil {
		_ = os.Remove(tmp.Name())
		if ctx.Err() == nil && dlCtx.Err() != nil {
			return "", "", 0, fmt.Errorf("download stalled (no data for %s): %w", a.dlIdle, err)
		}
		return "", "", 0, err
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), n, nil
}

// stallReader wraps a response body and invokes reset after any read that
// returns data, letting a caller's watchdog distinguish a stalled stream from
// a slow-but-progressing one.
type stallReader struct {
	r     io.Reader
	reset func()
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.reset()
	}
	return n, err
}

// Heartbeat reports liveness, free space, and the last synced run.
func (a *Agent) Heartbeat(ctx context.Context, pullToken, lastRunID string) error {
	free, total, err := diskUsage(a.cfg.StoreDir)
	if err != nil {
		return fmt.Errorf("disk usage: %w", err)
	}

	body, err := json.Marshal(map[string]any{
		"free_bytes":  free,
		"total_bytes": total,
		"last_run_id": lastRunID,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+pullToken)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("heartbeat failed: %s: %s", resp.Status, string(b))
	}

	if total > 0 {
		usedPct := 100 * float64(total-free) / float64(total)
		if usedPct >= a.cfg.CapacityWarnPct {
			a.cfg.Logger.Warn("store disk usage above warning threshold", "used_pct", usedPct, "threshold_pct", a.cfg.CapacityWarnPct)
		}
	}
	return nil
}

func diskUsage(dir string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	// #nosec G115 -- block counts/sizes are always non-negative on real filesystems.
	free = int64(st.Bavail) * int64(st.Bsize)
	total = int64(st.Blocks) * int64(st.Bsize)
	return free, total, nil
}

// Loop runs the enroll-then-pull-and-heartbeat cycle until ctx is canceled.
func (a *Agent) Loop(ctx context.Context) error {
	pullToken, err := a.EnsureEnrolled(ctx)
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}

	ticker := time.NewTicker(a.cfg.PullInterval)
	defer ticker.Stop()

	a.cycle(ctx, pullToken)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			a.cycle(ctx, pullToken)
		}
	}
}

func (a *Agent) cycle(ctx context.Context, pullToken string) {
	lastRunID, err := a.RunOnce(ctx, pullToken)
	if err != nil {
		a.cfg.Logger.Error("pull cycle had errors", "error", err)
	}
	if err := a.Heartbeat(ctx, pullToken, lastRunID); err != nil {
		a.cfg.Logger.Error("heartbeat failed", "error", err)
	}
}
