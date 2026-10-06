package roster

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestEnrollAuthenticateHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json")
	s := Open(path)

	enrollTok, err := s.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatalf("issue enrollment token: %v", err)
	}

	node, pullTok, err := s.Enroll(enrollTok, "pi-garage")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if node.Label != "pi-garage" || node.Status != StatusActive {
		t.Fatalf("unexpected node: %+v", node)
	}

	// The enrollment token is single-use: a second redemption must fail.
	if _, _, err := s.Enroll(enrollTok, "pi-garage-2"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken reusing a burned token, got %v", err)
	}

	got, err := s.Authenticate(pullTok)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != node.ID {
		t.Fatalf("authenticate returned wrong node: %+v", got)
	}

	if _, err := s.Authenticate("not-a-real-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for bad pull token, got %v", err)
	}

	if err := s.Heartbeat(node.ID, 100, 200, "run-20260101T000000Z"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	active, err := s.ActiveNodes()
	if err != nil {
		t.Fatalf("active nodes: %v", err)
	}
	if len(active) != 1 || active[0].FreeBytes != 100 || active[0].LastRunID != "run-20260101T000000Z" {
		t.Fatalf("unexpected active nodes: %+v", active)
	}
}

func TestEnrollExpiredToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json")
	s := Open(path)

	enrollTok, err := s.IssueEnrollmentToken(-time.Second) // already expired
	if err != nil {
		t.Fatalf("issue enrollment token: %v", err)
	}
	if _, _, err := s.Enroll(enrollTok, "pi-garage"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for expired token, got %v", err)
	}
}

func TestHeartbeatUnknownNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json")
	s := Open(path)
	if err := s.Heartbeat("does-not-exist", 1, 2, ""); err == nil {
		t.Fatal("expected error for unknown node")
	}
}

func enrollNode(t *testing.T, s *Store, label string) Node {
	t.Helper()
	tok, err := s.IssueEnrollmentToken(time.Hour)
	if err != nil {
		t.Fatalf("issue enrollment token: %v", err)
	}
	node, _, err := s.Enroll(tok, label)
	if err != nil {
		t.Fatalf("enroll %s: %v", label, err)
	}
	return node
}

func TestRetireAndGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json")
	s := Open(path)
	node := enrollNode(t, s, "pi-garage")

	got, err := s.Get(node.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusActive {
		t.Fatalf("expected active, got %v", got.Status)
	}

	if err := s.Retire(node.ID); err != nil {
		t.Fatalf("retire: %v", err)
	}
	got, err = s.Get(node.ID)
	if err != nil {
		t.Fatalf("get after retire: %v", err)
	}
	if got.Status != StatusRetired {
		t.Fatalf("expected retired, got %v", got.Status)
	}

	// A retired node can no longer authenticate.
	if _, err := s.Authenticate("anything"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}

	if err := s.Retire("does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound retiring unknown node, got %v", err)
	}
	if _, err := s.Get("does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound getting unknown node, got %v", err)
	}
}

func TestNextRoundRobin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roster.json")
	s := Open(path)

	if _, err := s.NextRoundRobin(); err == nil {
		t.Fatal("expected error with no active nodes")
	}

	a := enrollNode(t, s, "a")
	b := enrollNode(t, s, "b")
	c := enrollNode(t, s, "c")

	ids := []string{a.ID, b.ID, c.ID}
	sortedIDs := append([]string(nil), ids...)
	// NextRoundRobin orders active nodes by ID; replicate the same ordering
	// here so the expected rotation sequence is deterministic regardless of
	// the random IDs generated above.
	for i := 0; i < len(sortedIDs); i++ {
		for j := i + 1; j < len(sortedIDs); j++ {
			if sortedIDs[j] < sortedIDs[i] {
				sortedIDs[i], sortedIDs[j] = sortedIDs[j], sortedIDs[i]
			}
		}
	}

	var got []string
	for i := 0; i < 6; i++ {
		n, err := s.NextRoundRobin()
		if err != nil {
			t.Fatalf("next round robin: %v", err)
		}
		got = append(got, n.ID)
	}
	want := append(append([]string{}, sortedIDs...), sortedIDs...)
	if len(got) != len(want) {
		t.Fatalf("unexpected rotation length: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rotation mismatch at %d: got %v want %v", i, got, want)
		}
	}

	// Retiring the current cursor node must not break rotation.
	if err := s.Retire(sortedIDs[0]); err != nil {
		t.Fatalf("retire: %v", err)
	}
	n, err := s.NextRoundRobin()
	if err != nil {
		t.Fatalf("next round robin after retire: %v", err)
	}
	if n.ID == sortedIDs[0] {
		t.Fatalf("retired node must not be assigned: %v", n)
	}
}
