package health

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// runStatusFile records the outcome of the most recent packaging run.
const runStatusFile = ".last-run.json"

// RunStatus is the outcome of the most recent packaging run. Error is empty
// on success.
type RunStatus struct {
	FinishedAt time.Time `json:"finished_at"`
	RunID      string    `json:"run_id,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// WriteRunStatus atomically replaces the run status in outputDir.
func WriteRunStatus(outputDir string, s RunStatus) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(outputDir, runStatusFile+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(outputDir, runStatusFile))
}

// ReadRunStatus returns the recorded run status, or nil if no run has
// finished yet.
func ReadRunStatus(outputDir string) (*RunStatus, error) {
	b, err := os.ReadFile(filepath.Join(outputDir, runStatusFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s RunStatus
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
