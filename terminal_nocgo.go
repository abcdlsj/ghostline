//go:build !cgo || (!darwin && !linux) || ((darwin || linux) && !amd64 && !arm64)

package ghostline

import "fmt"

type vtTerminal struct{}

func newVTTerminal(int, int) (*vtTerminal, error) {
	return nil, fmt.Errorf("%w: libghostty-vt is unavailable for this build", ErrUnavailable)
}

func newVTTerminalWithOptions(int, int, vtTerminalOptions) (*vtTerminal, error) {
	return nil, fmt.Errorf("%w: libghostty-vt is unavailable for this build", ErrUnavailable)
}

// Feed is a no-op on an unavailable terminal.
func (*vtTerminal) Feed([]byte) {}

// Resize is a no-op on an unavailable terminal.
func (*vtTerminal) Resize(int, int) {}

// Snapshot reports that libghostty-vt is unavailable in this build.
func (*vtTerminal) Snapshot() ([]byte, error) {
	return nil, fmt.Errorf("%w: libghostty-vt is unavailable for this build", ErrUnavailable)
}

// EncodeState reports that libghostty-vt is unavailable in this build.
func (*vtTerminal) EncodeState() ([]byte, error) {
	return nil, fmt.Errorf("%w: libghostty-vt is unavailable for this build", ErrUnavailable)
}

// encodeAtomicState reports that libghostty-vt is unavailable in this build.
func (*vtTerminal) encodeAtomicState() ([]byte, error) {
	return nil, fmt.Errorf("%w: libghostty-vt is unavailable for this build", ErrUnavailable)
}

// RestoreState reports that libghostty-vt is unavailable in this build.
func (*vtTerminal) RestoreState([]byte) error {
	return fmt.Errorf("%w: libghostty-vt is unavailable for this build", ErrUnavailable)
}

// Close is a no-op on an unavailable terminal.
func (*vtTerminal) Close() {}
