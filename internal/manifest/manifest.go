// Package manifest defines the run manifest that accompanies every backup
// package: plaintext metadata that lets nodes and operators verify and track a
// run without decrypting it.
package manifest

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the manifest schema version. Bump on breaking changes.
const SchemaVersion = 1

// Distribution scheme names (§8).
const (
	SchemeReplicateAll = "replicate-all"
	SchemeRoundRobin   = "round-robin"
)

// Tool records which program produced the manifest.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// EntrySummary summarizes one top-level entry of the source tree.
type EntrySummary struct {
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	FileCount int    `json:"file_count"`
}

// Package describes the encrypted artifact.
type Package struct {
	Name        string `json:"name"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	Compression string `json:"compression"`
	Encryption  string `json:"encryption"`
}

// Contents summarizes what the package contains (pre-compression sizes).
type Contents struct {
	FileCount  int            `json:"file_count"`
	TotalBytes int64          `json:"total_bytes"`
	TopLevel   []EntrySummary `json:"top_level"`
}

// Distribution records which nodes this run was assigned to (§8). For
// "replicate-all" every active node at pull time is eligible regardless of
// AssignedNodeIDs; for "round-robin" only the listed node IDs may pull it.
type Distribution struct {
	Scheme          string   `json:"scheme"`
	AssignedNodeIDs []string `json:"assigned_node_ids,omitempty"`
}

// Manifest is the full run manifest.
type Manifest struct {
	SchemaVersion int          `json:"schema_version"`
	RunID         string       `json:"run_id"`
	CreatedAt     string       `json:"created_at"`
	SourceDir     string       `json:"source_dir"`
	Package       Package      `json:"package"`
	Contents      Contents     `json:"contents"`
	Distribution  Distribution `json:"distribution"`
	Tool          Tool         `json:"tool"`
}

// New returns a manifest with the schema version and creation time populated.
func New(runID string) Manifest {
	return Manifest{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
}

// Marshal renders the manifest as indented JSON with a trailing newline. This
// exact byte sequence is what gets signed.
func (m Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
