package state

import (
	"bytes"
	"os"
	"path/filepath"
)

// Sent is the set of event fingerprints (id + content) pushed during the last
// successful cycle. DataHub appends rather than overwrites, so re-pushing an
// unchanged event stores another copy; the runner pushes only events whose
// fingerprint is absent. Losing the file is safe: an empty set re-sends the
// current window once, it never skips anything.
type Sent map[[16]byte]struct{}

var sentMagic = []byte("ldxsent1")

// SentPath places the cache next to the checkpoint, in a directory every
// deployment already makes writable.
func SentPath(stateFile string) string {
	return filepath.Join(filepath.Dir(stateFile), "sent.bin")
}

// LoadSent returns an empty set when the file is missing, unreadable or not
// in the expected format.
func LoadSent(path string) Sent {
	s := Sent{}

	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, sentMagic) || (len(data)-len(sentMagic))%16 != 0 {
		return s
	}

	for rec := data[len(sentMagic):]; len(rec) > 0; rec = rec[16:] {
		s[[16]byte(rec[:16])] = struct{}{}
	}

	return s
}

func SaveSent(path string, s Sent) error {
	data := make([]byte, 0, len(sentMagic)+16*len(s))
	data = append(data, sentMagic...)

	for fp := range s {
		data = append(data, fp[:]...)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}
