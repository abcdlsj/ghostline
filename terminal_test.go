package ghostline

import "testing"

// TestEncodeStateSupportsUnfinishedContinuation is the regression test for
// rolling upgrades of long-running TUIs. A feed that ends in the middle of an
// escape sequence used to make ghostty_snapshot_encode_alloc return
// GHOSTTY_INVALID_VALUE because continuation tracking was disabled; with
// tracking enabled before the feed, both encode and restore must succeed.
func TestEncodeStateSupportsUnfinishedContinuation(t *testing.T) {
	vt, err := NewVTTerminal(80, 24)
	if err != nil {
		t.Fatalf("NewVTTerminal: %v", err)
	}
	defer vt.Close()

	// A partial CSI sequence leaves the VT parser off ground.
	vt.Feed([]byte("\x1b[31"))
	encoded, err := vt.EncodeState()
	if err != nil {
		t.Fatalf("EncodeState with unfinished continuation: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("EncodeState returned an empty snapshot")
	}

	restored, err := NewVTTerminal(80, 24)
	if err != nil {
		t.Fatalf("NewVTTerminal restore target: %v", err)
	}
	defer restored.Close()
	if err := restored.RestoreState(encoded); err != nil {
		t.Fatalf("RestoreState of unfinished continuation: %v", err)
	}
}
