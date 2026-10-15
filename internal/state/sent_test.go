package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSentRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sent.bin")
	want := Sent{{1}: {}, {2, 3}: {}, {0xff}: {}}

	if err := SaveSent(path, want); err != nil {
		t.Fatal(err)
	}

	got := LoadSent(path)
	if len(got) != len(want) {
		t.Fatalf("loaded %d fingerprints, want %d", len(got), len(want))
	}

	for fp := range want {
		if _, ok := got[fp]; !ok {
			t.Fatalf("fingerprint %x lost in round trip", fp)
		}
	}
}

func TestLoadSentIsEmptyWhenUnusable(t *testing.T) {
	dir := t.TempDir()

	for name, data := range map[string][]byte{
		"wrong-magic": []byte("notmagic0123456789abcdef"),
		"truncated":   append([]byte("ldxsent1"), 1, 2, 3),
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}

		if got := LoadSent(path); len(got) != 0 {
			t.Errorf("%s: loaded %d fingerprints, want 0", name, len(got))
		}
	}

	if got := LoadSent(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Errorf("missing file: loaded %d fingerprints, want 0", len(got))
	}
}
