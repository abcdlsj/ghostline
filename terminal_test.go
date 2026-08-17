package ghostline

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func newTestVTTerminal(t *testing.T, options VTTerminalOptions) *VTTerminal {
	t.Helper()
	vt, err := NewVTTerminalWithOptions(80, 24, options)
	if errors.Is(err, ErrUnavailable) {
		t.Skip("libghostty-vt requires cgo")
	}
	if err != nil {
		t.Fatalf("NewVTTerminalWithOptions: %v", err)
	}
	return vt
}

func TestVTTerminalOptionsResolveDefaults(t *testing.T) {
	if got := (VTTerminalOptions{}).resolvedScrollbackMaxBytes(); got != DefaultVTScrollbackMaxBytes {
		t.Fatalf("default scrollback = %d, want %d", got, DefaultVTScrollbackMaxBytes)
	}
	const configuredLimit = 4 << 20
	if got := (VTTerminalOptions{
		ScrollbackMaxBytes: configuredLimit,
	}).resolvedScrollbackMaxBytes(); got != configuredLimit {
		t.Fatalf("configured scrollback = %d, want %d", got, configuredLimit)
	}
}

func TestVTTerminalOptionsBoundRenderedHistory(t *testing.T) {
	small := newTestVTTerminal(t, VTTerminalOptions{
		ScrollbackMaxBytes: 1,
	})
	defer small.Close()
	large := newTestVTTerminal(t, VTTerminalOptions{
		ScrollbackMaxBytes: 16 << 20,
	})
	defer large.Close()

	var output bytes.Buffer
	for index := 0; index < 12_000; index++ {
		_, _ = fmt.Fprintf(&output, "scrollback-marker-%05d %s\r\n", index, "01234567890123456789012345678901234567890123456789")
	}
	small.Feed(output.Bytes())
	large.Feed(output.Bytes())

	smallSnapshot, err := small.Snapshot()
	if err != nil {
		t.Fatalf("small snapshot: %v", err)
	}
	largeSnapshot, err := large.Snapshot()
	if err != nil {
		t.Fatalf("large snapshot: %v", err)
	}
	marker := []byte("scrollback-marker-00000")
	if bytes.Contains(smallSnapshot, marker) {
		t.Fatal("small scrollback retained the oldest marker")
	}
	if !bytes.Contains(largeSnapshot, marker) {
		t.Fatal("large scrollback lost the oldest marker")
	}
}

// TestEncodeStateSupportsUnfinishedContinuation is the regression test for
// rolling upgrades of long-running TUIs. A feed that ends in the middle of an
// escape sequence used to make ghostty_snapshot_encode_alloc return
// GHOSTTY_INVALID_VALUE because continuation tracking was disabled; with
// tracking enabled before the feed, both encode and restore must succeed.
func TestEncodeStateSupportsUnfinishedContinuation(t *testing.T) {
	vt := newTestVTTerminal(t, VTTerminalOptions{})
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

	restored := newTestVTTerminal(t, VTTerminalOptions{})
	defer restored.Close()
	if err := restored.RestoreState(encoded); err != nil {
		t.Fatalf("RestoreState of unfinished continuation: %v", err)
	}
}
