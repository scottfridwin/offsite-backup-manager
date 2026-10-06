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

// Manifest is the full run manifest.
type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	RunID         string   `json:"run_id"`
	CreatedAt     string   `json:"created_at"`
	SourceDir     string   `json:"source_dir"`
	Package       Package  `json:"package"`
	Contents      Contents `json:"contents"`
	Tool          Tool     `json:"tool"`
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
