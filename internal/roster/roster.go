// Package roster persists the set of enrolled backup nodes and the
// outstanding single-use enrollment tokens used to admit new ones. It is the
// Primary's source of truth for "who is allowed to pull what" (NODE_ROSTER_FILE).
package roster

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Status is the lifecycle state of a roster node.
type Status string

const (
	// StatusPending nodes have enrolled but not yet confirmed via a first
	// heartbeat. They may authenticate (to pull and heartbeat) but are not
	// counted as participants: they are excluded from distribution assignment
	// and from the health check's required-confirmers set until they heartbeat.
	// This keeps an enrolled-but-never-seen "phantom" from gating health or
	// receiving run assignments it can never confirm.
	StatusPending Status = "pending"
	// StatusActive nodes have confirmed at least once and participate in
	// distribution and health; they may pull + heartbeat.
	StatusActive Status = "active"
	// StatusRetired nodes have been decommissioned and can no longer authenticate.
	StatusRetired Status = "retired"
)

// ErrInvalidToken is returned when an enrollment or pull token does not match
// any known, usable entry.
var ErrInvalidToken = errors.New("invalid or expired token")

// ErrNotFound is returned when a node ID does not match any roster entry.
var ErrNotFound = errors.New("node not found")

// Node is one enrolled backup node.
type Node struct {
	ID         string    `json:"id"`
	Label      string    `json:"label"`
	TokenHash  string    `json:"token_hash"` // sha256(pull token), hex
	Status     Status    `json:"status"`
	EnrolledAt time.Time `json:"enrolled_at"`
	LastSeen   time.Time `json:"last_seen"`
	LastRunID  string    `json:"last_run_id,omitempty"`
	FreeBytes  int64     `json:"free_bytes,omitempty"`
	TotalBytes int64     `json:"total_bytes,omitempty"`
}

type pendingToken struct {
	TokenHash string    `json:"token_hash"`
	ExpiresAt time.Time `json:"expires_at"`
}

type document struct {
	Nodes   []Node         `json:"nodes"`
	Pending []pendingToken `json:"pending_tokens"`
	// RotationCursor is the ID of the last node assigned by round-robin
	// distribution, so successive runs keep advancing through the active set.
	RotationCursor string `json:"rotation_cursor,omitempty"`
}

// Store is a file-backed roster. Read-modify-write operations are serialized
// both within a process (mutex) and across processes (an advisory flock on a
// sidecar lock file), so a separate process such as
// `docker exec <primary> /app enroll-token` cannot race the running server.
type Store struct {
	mu   sync.Mutex
	path string
}

// Open returns a Store backed by the given flat JSON file. The file is
// created on first write if it does not already exist.
func Open(path string) *Store {
	return &Store{path: path}
}

func (s *Store) load() (document, error) {
	var doc document
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, err
	}
	if len(b) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return doc, fmt.Errorf("parse roster file %q: %w", s.path, err)
	}
	return doc, nil
}

// save writes doc atomically (temp file + rename) so a crash mid-write never
// leaves a truncated roster file.
func (s *Store) save(doc document) error {
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".roster-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o640); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}

// withFileLock runs fn while holding the in-process mutex and an advisory OS
// lock (flock) on a sidecar lock file, making read-modify-write safe across
// processes. The lock file is separate from the roster file so the atomic
// temp+rename save is unaffected.
func (s *Store) withFileLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("open roster lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock roster: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	return fn()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newNodeID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// IssueEnrollmentToken creates a new single-use, expiring token that a node
// can redeem via Enroll, and persists its hash (never the token itself).
func (s *Store) IssueEnrollmentToken(ttl time.Duration) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}

	err = s.withFileLock(func() error {
		doc, err := s.load()
		if err != nil {
			return err
		}
		doc.Pending = append(doc.Pending, pendingToken{
			TokenHash: hashToken(token),
			ExpiresAt: time.Now().UTC().Add(ttl),
		})
		return s.save(doc)
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Enroll redeems a one-time enrollment token: it burns the token and
// registers a new node (pending until its first heartbeat), returning its
// durable pull token (returned to the caller once and never stored in
// plaintext).
func (s *Store) Enroll(enrollToken, label string) (Node, string, error) {
	var node Node
	var pullToken string
	err := s.withFileLock(func() error {
		doc, err := s.load()
		if err != nil {
			return err
		}

		hash := hashToken(enrollToken)
		now := time.Now().UTC()
		idx := -1
		for i, p := range doc.Pending {
			if subtle.ConstantTimeCompare([]byte(p.TokenHash), []byte(hash)) == 1 && now.Before(p.ExpiresAt) {
				idx = i
				break
			}
		}
		if idx == -1 {
			return ErrInvalidToken
		}
		// Burn the token (remove it) regardless of what happens next.
		doc.Pending = append(doc.Pending[:idx], doc.Pending[idx+1:]...)

		nodeID, err := newNodeID()
		if err != nil {
			return err
		}
		pt, err := newToken()
		if err != nil {
			return err
		}
		pullToken = pt
		node = Node{
			ID:         nodeID,
			Label:      label,
			TokenHash:  hashToken(pt),
			Status:     StatusPending,
			EnrolledAt: now,
		}
		doc.Nodes = append(doc.Nodes, node)
		return s.save(doc)
	})
	if err != nil {
		return Node{}, "", err
	}
	return node, pullToken, nil
}

// Authenticate finds the active node owning pullToken.
func (s *Store) Authenticate(pullToken string) (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		return Node{}, err
	}
	hash := hashToken(pullToken)
	for _, n := range doc.Nodes {
		if (n.Status == StatusActive || n.Status == StatusPending) && subtle.ConstantTimeCompare([]byte(n.TokenHash), []byte(hash)) == 1 {
			return n, nil
		}
	}
	return Node{}, ErrInvalidToken
}

// Heartbeat records liveness, capacity, and sync progress for nodeID.
func (s *Store) Heartbeat(nodeID string, freeBytes, totalBytes int64, lastRunID string) error {
	return s.withFileLock(func() error {
		doc, err := s.load()
		if err != nil {
			return err
		}
		for i := range doc.Nodes {
			if doc.Nodes[i].ID == nodeID {
				// First heartbeat confirms a pending node into a real participant.
				if doc.Nodes[i].Status == StatusPending {
					doc.Nodes[i].Status = StatusActive
				}
				doc.Nodes[i].LastSeen = time.Now().UTC()
				doc.Nodes[i].FreeBytes = freeBytes
				doc.Nodes[i].TotalBytes = totalBytes
				if lastRunID != "" {
					doc.Nodes[i].LastRunID = lastRunID
				}
				return s.save(doc)
			}
		}
		return fmt.Errorf("heartbeat: unknown node %q", nodeID)
	})
}

// ActiveNodes returns all nodes with status active, for distribution
// assignment and health reporting.
func (s *Store) ActiveNodes() ([]Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []Node
	for _, n := range doc.Nodes {
		if n.Status == StatusActive {
			out = append(out, n)
		}
	}
	return out, nil
}

// AllNodes returns every roster entry (active and retired), sorted by
// enrollment order, for operator visibility (e.g. `primary nodes`).
func (s *Store) AllNodes() ([]Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		return nil, err
	}
	return doc.Nodes, nil
}

// Get returns the roster entry for nodeID.
func (s *Store) Get(nodeID string) (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		return Node{}, err
	}
	for _, n := range doc.Nodes {
		if n.ID == nodeID {
			return n, nil
		}
	}
	return Node{}, ErrNotFound
}

// Retire marks nodeID retired (§5.4): it can no longer authenticate or be
// assigned future runs. Its existing data on its own media is untouched —
// the Primary has no path to reach or wipe it.
func (s *Store) Retire(nodeID string) error {
	return s.withFileLock(func() error {
		doc, err := s.load()
		if err != nil {
			return err
		}
		for i := range doc.Nodes {
			if doc.Nodes[i].ID == nodeID {
				doc.Nodes[i].Status = StatusRetired
				return s.save(doc)
			}
		}
		return ErrNotFound
	})
}

// NextRoundRobin returns the next active node in rotation for round-robin
// distribution (§8) and persists the advanced cursor so subsequent runs keep
// cycling through the current active set. Active nodes are ordered by ID for
// a stable rotation.
func (s *Store) NextRoundRobin() (Node, error) {
	var next Node
	err := s.withFileLock(func() error {
		doc, err := s.load()
		if err != nil {
			return err
		}

		var active []Node
		for _, n := range doc.Nodes {
			if n.Status == StatusActive {
				active = append(active, n)
			}
		}
		if len(active) == 0 {
			return fmt.Errorf("round-robin: no active nodes")
		}
		sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })

		nextIdx := 0
		for i, n := range active {
			if n.ID == doc.RotationCursor {
				nextIdx = (i + 1) % len(active)
				break
			}
		}

		next = active[nextIdx]
		doc.RotationCursor = next.ID
		return s.save(doc)
	})
	if err != nil {
		return Node{}, err
	}
	return next, nil
}
