// Package capacity projects node storage growth per §7.3: average package
// size and historical run cadence derived from already-published runs, and
// each active node's time-to-full from its live free-space telemetry.
package capacity

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

// YearDuration approximates a year for growth-per-year projections.
const YearDuration = 365 * 24 * time.Hour

// Projection summarizes historical run size and cadence.
type Projection struct {
	RunCount        int
	AvgPackageBytes int64
	// AvgCadence is the mean time between consecutive published runs. Zero
	// (with HasCadence false) when fewer than two runs exist to derive it from.
	AvgCadence  time.Duration
	HasCadence  bool
	GrowthPerYr int64 // 0 when !HasCadence
}

// NodeProjection is one node's projected time until its store fills up.
type NodeProjection struct {
	NodeID    string
	Label     string
	FreeBytes int64
	// TimeToFull is 0 (with HasEstimate false) when it can't be estimated yet
	// (no cadence history, or AvgPackageBytes is 0).
	TimeToFull  time.Duration
	HasEstimate bool
}

// Compute derives a Projection from every complete, published run in outputDir.
func Compute(outputDir string) (Projection, error) {
	runs, err := publishedRuns(outputDir)
	if err != nil {
		return Projection{}, err
	}
	if len(runs) == 0 {
		return Projection{}, nil
	}

	var totalBytes int64
	for _, m := range runs {
		totalBytes += m.Package.Bytes
	}
	proj := Projection{
		RunCount:        len(runs),
		AvgPackageBytes: totalBytes / int64(len(runs)),
	}

	if len(runs) < 2 {
		return proj, nil
	}

	var totalGap time.Duration
	prev, err := time.Parse(time.RFC3339, runs[0].CreatedAt)
	if err != nil {
		return Projection{}, fmt.Errorf("parse run %s created_at: %w", runs[0].RunID, err)
	}
	for _, m := range runs[1:] {
		cur, err := time.Parse(time.RFC3339, m.CreatedAt)
		if err != nil {
			return Projection{}, fmt.Errorf("parse run %s created_at: %w", m.RunID, err)
		}
		totalGap += cur.Sub(prev)
		prev = cur
	}
	proj.AvgCadence = totalGap / time.Duration(len(runs)-1)
	proj.HasCadence = proj.AvgCadence > 0
	if proj.HasCadence {
		runsPerYear := float64(YearDuration) / float64(proj.AvgCadence)
		proj.GrowthPerYr = int64(float64(proj.AvgPackageBytes) * runsPerYear)
	}
	return proj, nil
}

// NodeProjections estimates time-to-full for each active node, using a
// previously computed Projection.
func NodeProjections(proj Projection, nodes []roster.Node) []NodeProjection {
	out := make([]NodeProjection, 0, len(nodes))
	for _, n := range nodes {
		np := NodeProjection{NodeID: n.ID, Label: n.Label, FreeBytes: n.FreeBytes}
		if proj.HasCadence && proj.AvgPackageBytes > 0 && n.FreeBytes > 0 {
			runsUntilFull := float64(n.FreeBytes) / float64(proj.AvgPackageBytes)
			np.TimeToFull = time.Duration(runsUntilFull * float64(proj.AvgCadence))
			np.HasEstimate = true
		}
		out = append(out, np)
	}
	return out
}

// publishedRuns returns every complete run's manifest (manifest + signature +
// package all present), sorted oldest-first.
func publishedRuns(outputDir string) ([]manifest.Manifest, error) {
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
	sort.Strings(runIDs)

	out := make([]manifest.Manifest, 0, len(runIDs))
	for _, id := range runIDs {
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
		out = append(out, m)
	}
	return out, nil
}
