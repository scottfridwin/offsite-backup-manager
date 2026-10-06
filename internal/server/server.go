// Package server implements the Primary's node-facing HTTP API: enrollment,
// run discovery, package download, and heartbeat. Every route is read-only
// from the node's perspective except enroll/heartbeat, which only ever append
// to the roster — nothing here can modify or delete a node's own store.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

// maxEnrollBodyBytes and maxHeartbeatBodyBytes bound request bodies on
// unauthenticated/high-frequency routes to blunt trivial abuse.
const (
	maxEnrollBodyBytes    = 4 << 10
	maxHeartbeatBodyBytes = 4 << 10
)

// packageNamePattern allowlists the exact artifact filenames a run produces,
// which also prevents any path traversal via the {name} route parameter.
var packageNamePattern = regexp.MustCompile(`^run-[0-9]{8}T[0-9]{6}Z\.(tar\.zst\.age|manifest\.json|manifest\.json\.minisig)$`)

// Config configures the node-facing HTTP handler.
type Config struct {
	// OutputDir is where the packaging pipeline writes run artifacts
	// (PACKAGE_OUTPUT_DIR). Served read-only.
	OutputDir string
	// Roster is the enrolled-node store.
	Roster *roster.Store
	// Logger receives request-handling diagnostics. Defaults to slog.Default().
	Logger *slog.Logger
}

// NewHandler returns the node-facing HTTP handler.
func NewHandler(cfg Config) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &server{cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", s.handleEnroll)
	mux.HandleFunc("GET /runs", s.withAuth(s.handleRuns))
	mux.HandleFunc("GET /packages/{name}", s.withAuth(s.handlePackage))
	mux.HandleFunc("POST /heartbeat", s.withAuth(s.handleHeartbeat))
	return mux
}

type server struct {
	cfg Config
}

// withAuth validates the per-node bearer pull token before delegating to next.
func (s *server) withAuth(next func(http.ResponseWriter, *http.Request, roster.Node)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		node, err := s.cfg.Roster.Authenticate(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next(w, r, node)
	}
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if token == "" {
		return "", false
	}
	return token, true
}

type enrollRequest struct {
	Token string `json:"token"`
	Label string `json:"label"`
}

type enrollResponse struct {
	NodeID    string `json:"node_id"`
	PullToken string `json:"pull_token"`
}

func (s *server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := decodeJSON(w, r, maxEnrollBodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" || req.Label == "" {
		writeError(w, http.StatusBadRequest, "token and label are required")
		return
	}

	node, pullToken, err := s.cfg.Roster.Enroll(req.Token, req.Label)
	if err != nil {
		if errors.Is(err, roster.ErrInvalidToken) {
			writeError(w, http.StatusForbidden, "invalid or expired enrollment token")
			return
		}
		s.cfg.Logger.Error("enroll failed", "error", err)
		writeError(w, http.StatusInternalServerError, "enrollment failed")
		return
	}

	writeJSON(w, http.StatusOK, enrollResponse{NodeID: node.ID, PullToken: pullToken})
}

type runSummary struct {
	RunID        string `json:"run_id"`
	ManifestURL  string `json:"manifest_url"`
	SignatureURL string `json:"signature_url"`
	PackageURL   string `json:"package_url"`
}

func (s *server) handleRuns(w http.ResponseWriter, _ *http.Request, _ roster.Node) {
	runs, err := listRuns(s.cfg.OutputDir)
	if err != nil {
		s.cfg.Logger.Error("list runs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]runSummary{"runs": runs})
}

// listRuns scans OutputDir for complete runs (manifest + signature + package
// all present) and returns them sorted oldest-first.
func listRuns(outputDir string) ([]runSummary, error) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return nil, err
	}
	var runIDs []string
	for _, e := range entries {
		const suffix = ".manifest.json"
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "run-") || !strings.HasSuffix(name, suffix) {
			continue
		}
		runIDs = append(runIDs, strings.TrimSuffix(strings.TrimPrefix(name, "run-"), suffix))
	}
	sort.Strings(runIDs)

	out := make([]runSummary, 0, len(runIDs))
	for _, id := range runIDs {
		pkg := "run-" + id + ".tar.zst.age"
		manifest := "run-" + id + ".manifest.json"
		sig := manifest + ".minisig"
		if !fileExists(filepath.Join(outputDir, pkg)) || !fileExists(filepath.Join(outputDir, sig)) {
			continue // skip runs still being written
		}
		out = append(out, runSummary{
			RunID:        id,
			ManifestURL:  "/packages/" + manifest,
			SignatureURL: "/packages/" + sig,
			PackageURL:   "/packages/" + pkg,
		})
	}
	return out, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (s *server) handlePackage(w http.ResponseWriter, r *http.Request, _ roster.Node) {
	name := r.PathValue("name")
	if !packageNamePattern.MatchString(name) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	path := filepath.Join(s.cfg.OutputDir, name)
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "stat failed")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

type heartbeatRequest struct {
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
	LastRunID  string `json:"last_run_id"`
}

func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request, node roster.Node) {
	var req heartbeatRequest
	if err := decodeJSON(w, r, maxHeartbeatBodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.cfg.Roster.Heartbeat(node.ID, req.FreeBytes, req.TotalBytes, req.LastRunID); err != nil {
		s.cfg.Logger.Error("heartbeat failed", "error", err, "node_id", node.ID)
		writeError(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
