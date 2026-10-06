package manifest

import (
	"encoding/json"
	"testing"
)

func TestMarshalRoundTrip(t *testing.T) {
	m := New("20260101T000000Z")
	m.SourceDir = "/backups"
	m.Package = Package{Name: "run-x.tar.zst.age", Bytes: 123, SHA256: "abc", Compression: "zstd", Encryption: "age"}
	m.Contents = Contents{FileCount: 2, TotalBytes: 50, TopLevel: []EntrySummary{{Name: "svc", Bytes: 50, FileCount: 2}}}
	m.Tool = Tool{Name: "offsite-backup-manager", Version: "test"}

	b, err := m.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Fatalf("expected trailing newline")
	}

	var got Manifest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("schema version = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if got.RunID != m.RunID || got.Package.SHA256 != "abc" || got.Contents.FileCount != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}
