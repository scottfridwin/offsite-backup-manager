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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// incomingDirName holds in-progress files. Finished files are hard-linked
// from here into StoreDir, which only ever gains entries, so StoreDir itself
// can be made append-only (`chattr +a`) by the host.
const incomingDirName = ".incoming"

// spaceReserve is kept free on the store beyond a package's size.
const spaceReserve = 64 << 20

// Bounds on what the agent reads from the Primary into memory (packages are
// streamed to disk instead), so a misbehaving Primary can't exhaust a node.
const (
	maxResponseBytes  = 8 << 20
	maxErrorBodyBytes = 4 << 10
)

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

	lastRunID atomic.Value // string: newest run held, reported by heartbeats
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
				// Packages are encrypted (incompressible); a proxy compressing
				// them only costs CPU and hides Content-Length.
				DisableCompression: true,
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
	incoming := filepath.Join(cfg.StoreDir, incomingDirName)
	if err := os.MkdirAll(incoming, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", incoming, err)
	}
	if err := checkStoreWritable(incoming); err != nil {
		return nil, err
	}

	a := &Agent{
		cfg:      cfg,
		client:   cfg.HTTPClient,
		dlClient: dlClient,
		dlIdle:   cfg.DownloadIdleTimeout,
		verifier: verifier,
		baseURL:  "https://" + cfg.BackupHost,
	}
	a.lastRunID.Store(a.newestStoredRun())
	a.cleanIncoming()
	return a, nil
}

func (a *Agent) incomingPath(name string) string {
	return filepath.Join(a.cfg.StoreDir, incomingDirName, name)
}

// newestStoredRun returns the newest run whose package is in the store.
func (a *Agent) newestStoredRun() string {
	matches, _ := filepath.Glob(filepath.Join(a.cfg.StoreDir, "run-*.tar.zst.age"))
	newest := ""
	for _, m := range matches {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "run-"), ".tar.zst.age")
		if manifest.ValidRunID(id) && id > newest {
			newest = id
		}
	}
	return newest
}

// cleanIncoming removes what an interrupted cycle left behind, keeping only
// partial downloads of runs not yet stored (they are resumed).
func (a *Agent) cleanIncoming() {
	entries, err := os.ReadDir(filepath.Join(a.cfg.StoreDir, incomingDirName))
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".partial") && !fileExists(filepath.Join(a.cfg.StoreDir, strings.TrimSuffix(name, ".partial"))) {
			continue
		}
		_ = os.RemoveAll(a.incomingPath(name))
	}
	// Downloads from before .incoming existed.
	stale, _ := filepath.Glob(filepath.Join(a.cfg.StoreDir, ".download-*.tmp"))
	for _, p := range stale {
		_ = os.Remove(p)
	}
}

func (a *Agent) credentialPath() string {
	return filepath.Join(a.cfg.StoreDir, credentialFileName)
}

// checkStoreWritable fails fast with actionable guidance when STORE_DIR is not
// writable by the (nonroot, uid 65532) container user. The usual cause is a
// rootless bind mount still owned by the host user; catching it here avoids a
// confusing mid-enrollment "permission denied" that would also burn the
// one-time enrollment token.
func checkStoreWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".writecheck-*")
	if err != nil {
		return fmt.Errorf("store directory %q is not writable by this container user: %w; "+
			"on rootless podman/docker make it owned by the container user, e.g. "+
			"`podman unshare chown -R 65532:65532 <host store path>`", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
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
		return "", fmt.Errorf("enroll failed: %s: %s", resp.Status, errorBody(resp))
	}

	var out struct {
		NodeID    string `json:"node_id"`
		PullToken string `json:"pull_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode enroll response: %w", err)
	}

	cred := credential{NodeID: out.NodeID, PullToken: out.PullToken}
	credBytes, err := json.Marshal(cred)
	if err != nil {
		return "", err
	}
	if err := a.writeOnce(credentialFileName, credBytes, 0o600); err != nil {
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

// validate rejects a run whose ID or URLs differ from what the Primary
// produces. The ID becomes part of store file names and the URLs are appended
// to the Primary's base URL (carrying the pull token), so neither may be
// trusted as-is: the run listing itself is not signed.
func (r runSummary) validate() error {
	if !manifest.ValidRunID(r.RunID) {
		return fmt.Errorf("invalid run id %q", r.RunID)
	}
	base := "/packages/run-" + r.RunID
	if r.ManifestURL != base+".manifest.json" ||
		r.SignatureURL != base+".manifest.json.minisig" ||
		r.PackageURL != base+".tar.zst.age" {
		return fmt.Errorf("unexpected artifact URLs for run %s", r.RunID)
	}
	return nil
}

// errorBody returns the start of a non-success response body for error messages.
func errorBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return string(b)
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
		if err := run.validate(); err != nil {
			a.cfg.Logger.Error("rejected run from primary", "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// The signature is stored last, so its presence means the run is complete.
		if fileExists(filepath.Join(a.cfg.StoreDir, "run-"+run.RunID+".manifest.json.minisig")) {
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
		return nil, fmt.Errorf("fetch runs: %s: %s", resp.Status, errorBody(resp))
	}
	var out struct {
		Runs []runSummary `json:"runs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
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

	base := "run-" + run.RunID
	// A previous cycle stored the package but stopped before its manifest.
	if fileExists(filepath.Join(a.cfg.StoreDir, base+".tar.zst.age")) {
		return a.storeManifest(base, manifestBytes, sigBytes)
	}

	pkgTmp, pkgSum, pkgSize, err := a.downloadPackage(ctx, pullToken, run, m.Package)
	if err != nil {
		return fmt.Errorf("fetch package: %w", err)
	}

	if pkgSum != m.Package.SHA256 || (m.Package.Bytes != 0 && pkgSize != m.Package.Bytes) {
		_ = os.Remove(pkgTmp) // corrupt: don't resume from it
		if pkgSum != m.Package.SHA256 {
			return fmt.Errorf("package checksum mismatch for run %s: got %s want %s", run.RunID, pkgSum, m.Package.SHA256)
		}
		return fmt.Errorf("package size mismatch for run %s: got %d want %d", run.RunID, pkgSize, m.Package.Bytes)
	}

	if err := a.commitFile(pkgTmp, base+".tar.zst.age", 0o400); err != nil {
		return err
	}
	return a.storeManifest(base, manifestBytes, sigBytes)
}

func (a *Agent) storeManifest(base string, manifestBytes, sigBytes []byte) error {
	if err := a.writeOnce(base+".manifest.json", manifestBytes, 0o400); err != nil {
		return err
	}
	return a.writeOnce(base+".manifest.json.minisig", sigBytes, 0o400)
}

// writeOnce stores data as relName (relative to StoreDir) with the given mode
// unless it already exists. It never overwrites or deletes a stored file.
func (a *Agent) writeOnce(relName string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Join(a.cfg.StoreDir, incomingDirName), "write-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	return a.commitFile(tmp.Name(), relName, mode)
}

// commitFile publishes a finished file from the incoming dir as relName. A
// hard link (unlike rename) never replaces an existing entry and only adds one,
// so it also works when StoreDir is append-only.
func (a *Agent) commitFile(tmpPath, relName string, mode os.FileMode) error {
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err := os.Link(tmpPath, filepath.Join(a.cfg.StoreDir, relName)); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Remove(tmpPath)
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
		return nil, fmt.Errorf("%s: %s: %s", path, resp.Status, errorBody(resp))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxResponseBytes {
		return nil, fmt.Errorf("%s: response larger than %d bytes", path, maxResponseBytes)
	}
	return b, nil
}

// downloadPackage streams a run's package into a partial file in the incoming
// dir and returns its path, hex sha256 and size. A partial file left by an
// interrupted attempt is resumed with a Range request. It refuses to start
// when the store can't hold the rest of the package. The transfer is bounded
// only by an idle/stall timeout (no whole-request deadline).
func (a *Agent) downloadPackage(ctx context.Context, pullToken string, run runSummary, want manifest.Package) (string, string, int64, error) {
	partial := a.incomingPath("run-" + run.RunID + ".tar.zst.age.partial")
	f, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = f.Close() }()

	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", "", 0, err
	}
	if want.Bytes > 0 && offset > want.Bytes {
		if offset, err = restart(f); err != nil {
			return "", "", 0, err
		}
	}

	if want.Bytes > 0 {
		free, _, err := diskUsage(a.cfg.StoreDir)
		if err != nil {
			return "", "", 0, fmt.Errorf("disk usage: %w", err)
		}
		if need := want.Bytes - offset + spaceReserve; free < need {
			return "", "", 0, fmt.Errorf("not enough free space for run %s: need %d bytes, %d free", run.RunID, need, free)
		}
	}

	h := sha256.New()
	if offset > 0 {
		if _, err := io.Copy(h, io.NewSectionReader(f, 0, offset)); err != nil {
			return "", "", 0, err
		}
	}
	if want.Bytes > 0 && offset == want.Bytes {
		return partial, hex.EncodeToString(h.Sum(nil)), offset, nil
	}

	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, a.baseURL+run.PackageURL, nil)
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+pullToken)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := a.dlClient.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0 &&
		strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", offset)):
		a.cfg.Logger.Info("resuming package download", "run_id", run.RunID, "offset", offset)
	case resp.StatusCode == http.StatusOK:
		if offset > 0 { // the Primary ignored the range: start over
			if offset, err = restart(f); err != nil {
				return "", "", 0, err
			}
			h.Reset()
		}
	default:
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			_, _ = restart(f)
		}
		return "", "", 0, fmt.Errorf("%s: %s: %s", run.PackageURL, resp.Status, errorBody(resp))
	}

	// Cancel the request if the body stalls (no bytes) for dlIdle; each
	// successful read resets the watchdog.
	idle := time.AfterFunc(a.dlIdle, cancel)
	defer idle.Stop()
	body := &stallReader{r: resp.Body, reset: func() { idle.Reset(a.dlIdle) }}

	n, err := io.Copy(io.MultiWriter(f, h), body)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if err != nil {
		// The partial file is kept and resumed next cycle.
		if ctx.Err() == nil && dlCtx.Err() != nil {
			return "", "", 0, fmt.Errorf("download stalled (no data for %s) after %d bytes: %w", a.dlIdle, offset+n, err)
		}
		return "", "", 0, err
	}
	return partial, hex.EncodeToString(h.Sum(nil)), offset + n, nil
}

func restart(f *os.File) (int64, error) {
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	return f.Seek(0, io.SeekStart)
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
		return fmt.Errorf("heartbeat failed: %s: %s", resp.Status, errorBody(resp))
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
// Heartbeats also run on their own ticker, so a long download doesn't make
// the node look dead to the Primary.
func (a *Agent) Loop(ctx context.Context) error {
	pullToken, err := a.EnsureEnrolled(ctx)
	if err != nil {
		return fmt.Errorf("enroll: %w", err)
	}

	go func() {
		t := time.NewTicker(a.cfg.PullInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.heartbeat(ctx, pullToken)
			}
		}
	}()

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
	if _, err := a.RunOnce(ctx, pullToken); err != nil {
		a.cfg.Logger.Error("pull cycle had errors", "error", err)
	}
	a.lastRunID.Store(a.newestStoredRun())
	a.heartbeat(ctx, pullToken)
}

func (a *Agent) heartbeat(ctx context.Context, pullToken string) {
	last, _ := a.lastRunID.Load().(string)
	if err := a.Heartbeat(ctx, pullToken, last); err != nil && ctx.Err() == nil {
		a.cfg.Logger.Error("heartbeat failed", "error", err)
	}
}
