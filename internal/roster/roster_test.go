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
