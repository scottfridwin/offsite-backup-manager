package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestRunSummaryValidate(t *testing.T) {
	good := runSummary{
		RunID:        "20260101T000000Z",
		ManifestURL:  "/packages/run-20260101T000000Z.manifest.json",
		SignatureURL: "/packages/run-20260101T000000Z.manifest.json.minisig",
		PackageURL:   "/packages/run-20260101T000000Z.tar.zst.age",
	}
	if err := good.validate(); err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}

	cases := map[string]func(r *runSummary){
		"traversal run id": func(r *runSummary) { r.RunID = "../../etc/x" },
		"empty run id":     func(r *runSummary) { r.RunID = "" },
		"foreign host":     func(r *runSummary) { r.PackageURL = "@evil.example/run.tar.zst.age" },
		"absolute url":     func(r *runSummary) { r.ManifestURL = "https://evil.example/m.json" },
		"other run":        func(r *runSummary) { r.SignatureURL = "/packages/run-20250101T000000Z.manifest.json.minisig" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := good
			mutate(&r)
			if err := r.validate(); err == nil {
				t.Fatalf("expected %+v to be rejected", r)
			}
		})
	}
}

// A Primary advertising a run with a malicious ID or URLs must not make the
// node request anything or write outside its store.
func TestAgentRejectsMaliciousRunListing(t *testing.T) {
	pubPath, _ := writeMinisignKeyPair(t)
	var otherRequests atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runs" {
			otherRequests.Add(1)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string][]runSummary{"runs": {
			{
				RunID:        "../escape",
				ManifestURL:  "/packages/run-../escape.manifest.json",
				SignatureURL: "/packages/run-../escape.manifest.json.minisig",
				PackageURL:   "/packages/run-../escape.tar.zst.age",
			},
			{
				RunID:        "20260101T000000Z",
				ManifestURL:  "@evil.example/m.json",
				SignatureURL: "@evil.example/m.json.minisig",
				PackageURL:   "@evil.example/p.tar.zst.age",
			},
		}})
	}))
	defer ts.Close()

	parent := t.TempDir()
	store := filepath.Join(parent, "store")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{
		BackupHost:         "example.invalid",
		StoreDir:           store,
		MinisignPubKeyFile: pubPath,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	a.baseURL = ts.URL
	a.client = ts.Client()
	a.dlClient = ts.Client()

	last, err := a.RunOnce(context.Background(), "token")
	if err == nil {
		t.Fatal("expected the malicious runs to be rejected")
	}
	if last != "" {
		t.Fatalf("no run should count as synced, got %q", last)
	}
	if n := otherRequests.Load(); n != 0 {
		t.Fatalf("agent made %d requests for rejected runs", n)
	}
	if entries, _ := os.ReadDir(store); len(entries) != 1 || entries[0].Name() != incomingDirName {
		t.Fatalf("store should hold only the empty incoming dir, has %v", entries)
	}
	if entries, _ := os.ReadDir(filepath.Join(store, incomingDirName)); len(entries) != 0 {
		t.Fatalf("incoming dir should be empty, has %v", entries)
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 1 {
		t.Fatalf("nothing may be written next to the store, found %v", entries)
	}
}
