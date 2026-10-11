package manifest

import "testing"

func TestValidRunID(t *testing.T) {
	for id, want := range map[string]bool{
		"20260101T000000Z":        true,
		"":                        false,
		"20260101T000000":         false,
		"20260101T000000Z/../x":   false,
		"../20260101T000000Z":     false,
		"2026010AT000000Z":        false,
		"20260101T000000Z\n":      false,
		"20260101T000000Z.tar.gz": false,
	} {
		if got := ValidRunID(id); got != want {
			t.Errorf("ValidRunID(%q) = %v, want %v", id, got, want)
		}
	}
}
