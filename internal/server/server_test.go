package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

func writeRun(t *testing.T, outputDir, runID string) {
	t.Helper()
	files := map[string]string{
		"run-" + runID + ".tar.zst.age":           "ciphertext",
		"run-" + runID + ".manifest.json":         `{"run_id":"` + runID + `"}` + "\n",
		"run-" + runID + ".manifest.json.minisig": "signature",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(outputDir, name), []byte(content), 0o640); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestEnrollRunsPackagesHeartbeat(t *testing.T) {
	outputDir := t.TempDir()
	rosterPath := filepath.Join(t.TempDir(), "roster.json")
	store := roster.Open(rosterPath)
	h := NewHandler(Config{OutputDir: outputDir, Roster: store})
	ts := httptest.NewServer(h)
	defer ts.Close()

	writeRun(t, outputDir, "20260101T000000Z")

	enrollTok, err := store.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	enrollBody, _ := json.Marshal(map[string]string{"token": enrollTok, "label": "pi-garage"})
	resp, err := http.Post(ts.URL+"/enroll", "application/json", bytes.NewReader(enrollBody))
	if err != nil {
		t.Fatalf("enroll request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("enroll status = %d body = %s", resp.StatusCode, b)
	}
	var enrollResp enrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&enrollResp); err != nil {
		t.Fatalf("decode enroll response: %v", err)
	}
	if enrollResp.PullToken == "" {
		t.Fatal("expected a non-empty pull token")
	}

	// Reusing the enrollment token must fail.
	resp2, err := http.Post(ts.URL+"/enroll", "application/json", bytes.NewReader(enrollBody))
	if err != nil {
		t.Fatalf("second enroll request: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 reusing enrollment token, got %d", resp2.StatusCode)
	}

	// /runs without auth is rejected.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/runs", nil)
	noAuthResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated runs request: %v", err)
	}
	_ = noAuthResp.Body.Close()
	if noAuthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", noAuthResp.StatusCode)
	}

	// /runs with the pull token lists the run we wrote.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/runs", nil)
	req.Header.Set("Authorization", "Bearer "+enrollResp.PullToken)
	runsResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("runs request: %v", err)
	}
	defer func() { _ = runsResp.Body.Close() }()
	var runsBody struct {
		Runs []runSummary `json:"runs"`
	}
	if err := json.NewDecoder(runsResp.Body).Decode(&runsBody); err != nil {
		t.Fatalf("decode runs response: %v", err)
	}
	if len(runsBody.Runs) != 1 || runsBody.Runs[0].RunID != "20260101T000000Z" {
		t.Fatalf("unexpected runs: %+v", runsBody.Runs)
	}

	// Download the package artifact.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+runsBody.Runs[0].PackageURL, nil)
	req.Header.Set("Authorization", "Bearer "+enrollResp.PullToken)
	pkgResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("package request: %v", err)
	}
	defer func() { _ = pkgResp.Body.Close() }()
	pkgBytes, _ := io.ReadAll(pkgResp.Body)
	if string(pkgBytes) != "ciphertext" {
		t.Fatalf("unexpected package body: %q", pkgBytes)
	}

	// Path traversal / unexpected filenames are rejected.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/packages/..%2F..%2Fetc%2Fpasswd", nil)
	req.Header.Set("Authorization", "Bearer "+enrollResp.PullToken)
	traversalResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("traversal request: %v", err)
	}
	_ = traversalResp.Body.Close()
	if traversalResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for traversal attempt, got %d", traversalResp.StatusCode)
	}

	// Heartbeat.
	hbBody, _ := json.Marshal(map[string]any{"free_bytes": 123, "total_bytes": 456, "last_run_id": "20260101T000000Z"})
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/heartbeat", bytes.NewReader(hbBody))
	req.Header.Set("Authorization", "Bearer "+enrollResp.PullToken)
	hbResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("heartbeat request: %v", err)
	}
	_ = hbResp.Body.Close()
	if hbResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", hbResp.StatusCode)
	}

	active, err := store.ActiveNodes()
	if err != nil {
		t.Fatalf("active nodes: %v", err)
	}
	if len(active) != 1 || active[0].FreeBytes != 123 {
		t.Fatalf("unexpected active nodes after heartbeat: %+v", active)
	}
}
