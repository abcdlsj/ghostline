package ghostline

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func newTestVTTerminalSize(t *testing.T, cols, rows int, options VTTerminalOptions) *VTTerminal {
	t.Helper()
	vt, err := NewVTTerminalWithOptions(cols, rows, options)
	if errors.Is(err, ErrUnavailable) {
		t.Skip("libghostty-vt requires cgo")
	}
	if err != nil {
		t.Fatalf("NewVTTerminalWithOptions: %v", err)
	}
	return vt
}

func newTestVTTerminal(t *testing.T, options VTTerminalOptions) *VTTerminal {
	return newTestVTTerminalSize(t, 80, 24, options)
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

func TestVTTerminalResizeKeepsSnapshotEncodableAfterWideBoundary(t *testing.T) {
	vt := newTestVTTerminalSize(t, 138, 42, VTTerminalOptions{})
	defer vt.Close()

	// The wide character occupies columns 123-124. Shrinking to 123 columns
	// reproduces the old no-reflow resize bug at the new right edge.
	vt.Feed([]byte("\x1b[?1049h\x1b[7;123H\xe5\x86\x99"))
	vt.Resize(123, 40)

	if _, err := vt.EncodeState(); err != nil {
		t.Fatalf("EncodeState after wide-boundary resize: %v", err)
	}
}

func TestVTTerminalResizeRepairsInactiveAlternateBoundary(t *testing.T) {
	vt := newTestVTTerminalSize(t, 138, 42, VTTerminalOptions{})
	defer vt.Close()

	// Leave the malformed wide pair on the inactive alternate screen while
	// keeping primary active, matching the migration failure observed in the
	// retained session.
	vt.Feed([]byte("primary\x1b[?1049h\x1b[7;123H\xe5\x86\x99\x1b[?1049l"))
	vt.Resize(123, 40)

	if _, err := vt.EncodeState(); err != nil {
		t.Fatalf("EncodeState with inactive alternate boundary: %v", err)
	}
	vt.Feed([]byte("P"))
	snapshot, err := vt.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after inactive alternate repair: %v", err)
	}
	if !bytes.Contains(snapshot, []byte("primaryP")) {
		t.Fatalf("active primary cursor moved during alternate repair: %q", snapshot)
	}
}

func TestVTTerminalResizeKeepsNoReflowPrimaryEncodable(t *testing.T) {
	vt := newTestVTTerminalSize(t, 138, 42, VTTerminalOptions{})
	defer vt.Close()

	vt.Feed([]byte("\x1b[?7l\x1b[7;123H\xe5\x86\x99"))
	vt.Resize(123, 40)

	if _, err := vt.EncodeState(); err != nil {
		t.Fatalf("EncodeState after no-reflow primary resize: %v", err)
	}
}
