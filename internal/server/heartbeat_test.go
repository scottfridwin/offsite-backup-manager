package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

func TestHeartbeatRejectsInvalidValues(t *testing.T) {
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	ts := httptest.NewServer(NewHandler(Config{OutputDir: t.TempDir(), Roster: store}))
	defer ts.Close()

	enrollTok, err := store.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	enrollBody, _ := json.Marshal(map[string]string{"token": enrollTok, "label": "pi-garage"})
	resp, err := http.Post(ts.URL+"/enroll", "application/json", bytes.NewReader(enrollBody))
	if err != nil {
		t.Fatalf("enroll request: %v", err)
	}
	var enrolled enrollResponse
	err = json.NewDecoder(resp.Body).Decode(&enrolled)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("decode enroll response: %v", err)
	}

	heartbeat := func(body map[string]any) int {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/heartbeat", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+enrolled.PullToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("heartbeat request: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	for name, body := range map[string]map[string]any{
		"negative free":      {"free_bytes": -1, "total_bytes": 10},
		"negative total":     {"free_bytes": 0, "total_bytes": -10},
		"free above total":   {"free_bytes": 11, "total_bytes": 10},
		"malformed last run": {"free_bytes": 1, "total_bytes": 10, "last_run_id": "99999999T999999Z-later"},
	} {
		if code := heartbeat(body); code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, code)
		}
	}

	// The rejected heartbeats did not activate the node; a valid one does.
	if code := heartbeat(map[string]any{"free_bytes": 1, "total_bytes": 10}); code != http.StatusNoContent {
		t.Fatalf("valid heartbeat: expected 204, got %d", code)
	}
	active, err := store.ActiveNodes()
	if err != nil {
		t.Fatalf("active nodes: %v", err)
	}
	if len(active) != 1 || active[0].FreeBytes != 1 || active[0].LastRunID != "" {
		t.Fatalf("unexpected active nodes: %+v", active)
	}
}
