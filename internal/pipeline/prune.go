package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/scottfridwin/offsite-backup-manager/internal/health"
	"github.com/scottfridwin/offsite-backup-manager/internal/manifest"
	"github.com/scottfridwin/offsite-backup-manager/internal/roster"
)

// prune removes published runs beyond the newest keep, but only runs every
// node required to hold them has confirmed: nodes keep the history (WORM),
// the Primary only needs what hasn't been delivered yet. A node that has never
// sent a heartbeat isn't waited for (it gets the newest runs once it starts).
// Callers must hold the output directory lock.
func prune(outputDir, rosterFile string, keep int) ([]string, error) {
	runIDs, err := publishedRuns(outputDir)
	if err != nil {
		return nil, err
	}
	if len(runIDs) <= keep {
		return nil, nil
	}

	var active []roster.Node
	if rosterFile != "" {
		if active, err = roster.Open(rosterFile).ActiveNodes(); err != nil {
			return nil, fmt.Errorf("list active nodes: %w", err)
		}
	}

	var pruned []string
	for _, id := range runIDs[:len(runIDs)-keep] {
		m, err := readManifest(outputDir, id)
		if err != nil {
			return pruned, err
		}
		if !deliveredToAll(m, active) {
			continue
		}
		base := filepath.Join(outputDir, "run-"+id)
		// Signature first: without it the run is no longer listed or served.
		for _, suffix := range []string{".manifest.json.minisig", ".tar.zst.age", ".manifest.json"} {
			if err := os.Remove(base + suffix); err != nil && !os.IsNotExist(err) {
				return pruned, err
			}
		}
		pruned = append(pruned, id)
	}
	return pruned, nil
}

func deliveredToAll(m manifest.Manifest, active []roster.Node) bool {
	for _, n := range health.RequiredNodes(m, active) {
		if !n.LastSeen.IsZero() && n.LastRunID < m.RunID {
			return false
		}
	}
	return true
}

// publishedRuns returns the IDs of complete runs, oldest first.
func publishedRuns(outputDir string) ([]string, error) {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "run-") || !strings.HasSuffix(name, ".manifest.json.minisig") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "run-"), ".manifest.json.minisig")
		if manifest.ValidRunID(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func readManifest(outputDir, id string) (manifest.Manifest, error) {
	var m manifest.Manifest
	b, err := os.ReadFile(filepath.Join(outputDir, "run-"+id+".manifest.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse manifest for run %s: %w", id, err)
	}
	return m, nil
}
