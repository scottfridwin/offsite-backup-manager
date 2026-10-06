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
	"sync"
	"time"
)

// Status is the lifecycle state of a roster node.
type Status string

const (
	// StatusActive nodes participate in distribution and may pull + heartbeat.
	StatusActive Status = "active"
	// StatusRetired nodes have been decommissioned and can no longer authenticate.
	StatusRetired Status = "retired"
)

// ErrInvalidToken is returned when an enrollment or pull token does not match
// any known, usable entry.
var ErrInvalidToken = errors.New("invalid or expired token")

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
}

// Store is a file-backed roster. It is safe for concurrent use by a single
// process (the Primary server); it is not designed for multi-process access.
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

	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		return "", err
	}
	doc.Pending = append(doc.Pending, pendingToken{
		TokenHash: hashToken(token),
		ExpiresAt: time.Now().UTC().Add(ttl),
	})
	if err := s.save(doc); err != nil {
		return "", err
	}
	return token, nil
}

// Enroll redeems a one-time enrollment token: it burns the token and
// registers a new active node, returning its durable pull token (returned to
// the caller once and never stored in plaintext).
func (s *Store) Enroll(enrollToken, label string) (Node, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		return Node{}, "", err
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
		return Node{}, "", ErrInvalidToken
	}
	// Burn the token (remove it) regardless of what happens next.
	doc.Pending = append(doc.Pending[:idx], doc.Pending[idx+1:]...)

	nodeID, err := newNodeID()
	if err != nil {
		return Node{}, "", err
	}
	pullToken, err := newToken()
	if err != nil {
		return Node{}, "", err
	}
	node := Node{
		ID:         nodeID,
		Label:      label,
		TokenHash:  hashToken(pullToken),
		Status:     StatusActive,
		EnrolledAt: now,
	}
	doc.Nodes = append(doc.Nodes, node)

	if err := s.save(doc); err != nil {
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
		if n.Status == StatusActive && subtle.ConstantTimeCompare([]byte(n.TokenHash), []byte(hash)) == 1 {
			return n, nil
		}
	}
	return Node{}, ErrInvalidToken
}

// Heartbeat records liveness, capacity, and sync progress for nodeID.
func (s *Store) Heartbeat(nodeID string, freeBytes, totalBytes int64, lastRunID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		return err
	}
	for i := range doc.Nodes {
		if doc.Nodes[i].ID == nodeID {
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
