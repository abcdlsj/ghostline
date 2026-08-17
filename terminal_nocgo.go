//go:build !cgo

package ghostline

import "fmt"

// VTTerminal is unavailable in builds that disable CGo.
type VTTerminal struct{}

// NewVTTerminal reports that libghostty-vt requires CGo.
func NewVTTerminal(int, int) (*VTTerminal, error) {
	return nil, fmt.Errorf("%w: libghostty-vt requires cgo", ErrUnavailable)
}

// NewVTTerminalWithOptions reports that libghostty-vt requires CGo.
func NewVTTerminalWithOptions(int, int, VTTerminalOptions) (*VTTerminal, error) {
	return nil, fmt.Errorf("%w: libghostty-vt requires cgo", ErrUnavailable)
}

// Feed is a no-op on an unavailable terminal.
func (*VTTerminal) Feed([]byte) {}

// Resize is a no-op on an unavailable terminal.
func (*VTTerminal) Resize(int, int) {}

// Snapshot reports that libghostty-vt requires CGo.
func (*VTTerminal) Snapshot() ([]byte, error) {
	return nil, fmt.Errorf("%w: libghostty-vt requires cgo", ErrUnavailable)
}

// EncodeState reports that libghostty-vt requires CGo.
func (*VTTerminal) EncodeState() ([]byte, error) {
	return nil, fmt.Errorf("%w: libghostty-vt requires cgo", ErrUnavailable)
}

// RestoreState reports that libghostty-vt requires CGo.
func (*VTTerminal) RestoreState([]byte) error {
	return fmt.Errorf("%w: libghostty-vt requires cgo", ErrUnavailable)
}

// Close is a no-op on an unavailable terminal.
func (*VTTerminal) Close() {}
