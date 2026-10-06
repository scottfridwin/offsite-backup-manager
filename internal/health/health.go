// Package health evaluates whether the Primary is healthy per §9.2: the most
// recent run must have built successfully within a configured interval, and —
// after a grace window — every node required by that run's distribution
// assignment must have confirmed pulling it via heartbeat. It is surfaced by
// `primary healthcheck`, suitable for a Docker HEALTHCHECK.
package health

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

// Config configures a health evaluation.
type Config struct {
	OutputDir        string
	Roster           *roster.Store
	RunInterval      time.Duration
	SyncGrace        time.Duration
	SpaceCriticalPct float64
	// Now returns the current time; overridable for tests. Defaults to time.Now.
	Now func() time.Time
}

// Result is the outcome of a health evaluation.
type Result struct {
	Healthy bool
	// Reasons explains why Healthy is false; empty when Healthy is true.
	Reasons []string
}

// Evaluate reports whether the Primary is currently healthy.
func Evaluate(cfg Config) (Result, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	latest, err := latestRun(cfg.OutputDir)
	if err != nil {
		return Result{}, err
	}
	if latest == nil {
		return Result{Healthy: false, Reasons: []string{"no successful run has been published yet"}}, nil
	}

	createdAt, err := time.Parse(time.RFC3339, latest.CreatedAt)
	if err != nil {
		return Result{}, fmt.Errorf("parse run %s created_at: %w", latest.RunID, err)
	}
	age := now().Sub(createdAt)

	var reasons []string
	if age > cfg.RunInterval {
		reasons = append(reasons, fmt.Sprintf("no run has succeeded within %s (last run %s was published %s ago)", cfg.RunInterval, latest.RunID, age.Round(time.Second)))
	}

	active, err := cfg.Roster.ActiveNodes()
	if err != nil {
		return Result{}, fmt.Errorf("list active nodes: %w", err)
	}

	// Required nodes only need to have confirmed once the grace window since
	// the latest run has elapsed (eventual consistency, §9.1).
	if age > cfg.SyncGrace {
		for _, node := range requiredNodes(*latest, active) {
			if node.LastRunID < latest.RunID {
				reasons = append(reasons, fmt.Sprintf("node %s (%s) has not confirmed run %s (last confirmed: %q)", node.Label, node.ID, latest.RunID, node.LastRunID))
			}
		}
	}

	for _, node := range active {
		if node.TotalBytes <= 0 {
			continue
		}
		usedPct := 100 * float64(node.TotalBytes-node.FreeBytes) / float64(node.TotalBytes)
		if usedPct >= cfg.SpaceCriticalPct {
			reasons = append(reasons, fmt.Sprintf("node %s (%s) is critically low on space (%.1f%% used)", node.Label, node.ID, usedPct))
		}
	}

	return Result{Healthy: len(reasons) == 0, Reasons: reasons}, nil
}

// requiredNodes returns the active nodes expected to have confirmed run m:
// every active node for replicate-all (or a manifest with no recorded
// scheme, for backward compatibility), or just the assigned node(s) for
// round-robin.
func requiredNodes(m manifest.Manifest, active []roster.Node) []roster.Node {
	if m.Distribution.Scheme != manifest.SchemeRoundRobin {
		return active
	}
	assigned := make(map[string]bool, len(m.Distribution.AssignedNodeIDs))
	for _, id := range m.Distribution.AssignedNodeIDs {
		assigned[id] = true
	}
	var out []roster.Node
	for _, n := range active {
		if assigned[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// latestRun returns the manifest of the most recently published, complete
// run (manifest + signature + package all present), or nil if none exist.
func latestRun(outputDir string) (*manifest.Manifest, error) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return nil, err
	}
	var runIDs []string
	for _, e := range entries {
		const suffix = ".manifest.json"
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "run-") || !strings.HasSuffix(name, suffix) {
			continue
		}
		runIDs = append(runIDs, strings.TrimSuffix(strings.TrimPrefix(name, "run-"), suffix))
	}
	if len(runIDs) == 0 {
		return nil, nil
	}
	sort.Strings(runIDs)

	for i := len(runIDs) - 1; i >= 0; i-- {
		id := runIDs[i]
		pkg := filepath.Join(outputDir, "run-"+id+".tar.zst.age")
		sig := filepath.Join(outputDir, "run-"+id+".manifest.json.minisig")
		if _, err := os.Stat(pkg); err != nil {
			continue
		}
		if _, err := os.Stat(sig); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(outputDir, "run-"+id+".manifest.json"))
		if err != nil {
			return nil, err
		}
		var m manifest.Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("parse manifest for run %s: %w", id, err)
		}
		return &m, nil
	}
	return nil, nil
}
