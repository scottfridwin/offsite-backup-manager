package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

func TestPackageRangeRequests(t *testing.T) {
	out := t.TempDir()
	name := "run-20260101T000000Z.tar.zst.age"
	if err := os.WriteFile(filepath.Join(out, name), []byte("0123456789"), 0o640); err != nil {
		t.Fatal(err)
	}
	store := roster.Open(filepath.Join(t.TempDir(), "roster.json"))
	tok, err := store.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, pull, err := store.Enroll(tok, "pi")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewHandler(Config{OutputDir: out, Roster: store}))
	defer ts.Close()

	get := func(rng string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/packages/"+name, nil)
		req.Header.Set("Authorization", "Bearer "+pull)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp, body := get("")
	if resp.StatusCode != http.StatusOK || body != "0123456789" || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("full GET: %d %q accept-ranges=%q", resp.StatusCode, body, resp.Header.Get("Accept-Ranges"))
	}
	resp, body = get("bytes=4-")
	if resp.StatusCode != http.StatusPartialContent || body != "456789" || resp.Header.Get("Content-Range") != "bytes 4-9/10" {
		t.Fatalf("resume GET: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Range"))
	}
	if resp, _ = get("bytes=10-"); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("range past end: %d, want 416", resp.StatusCode)
	}
	if resp, body = get("bytes=2-3"); resp.StatusCode != http.StatusOK || body != "0123456789" {
		t.Fatalf("unsupported range form should serve the whole file: %d %q", resp.StatusCode, body)
	}
}
