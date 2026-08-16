//go:build cgo

package ghostline

/*
#cgo CFLAGS: -I${SRCDIR}/third_party/include
#cgo darwin,arm64 LDFLAGS: -L${SRCDIR}/third_party/lib -lghostty-vt -Wl,-rpath,${SRCDIR}/third_party/lib
#cgo darwin,!arm64 LDFLAGS: -lghostty-vt
#cgo !darwin LDFLAGS: -lghostty-vt
#include <stdlib.h>
#include <ghostty/vt.h>
#include <ghostty/vt/snapshot.h>
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// VTTerminal is a libghostty-vt terminal emulator that renders raw PTY bytes
// into a complete screen snapshot (visible grid + scrollback) with SGR styles
// preserved. It is the server-side counterpart of the Ghostty client, so a
// replayed snapshot matches exactly what the client would have rendered.
type VTTerminal struct {
	mu       sync.Mutex
	terminal C.GhosttyTerminal
}

// NewVTTerminal creates a terminal emulator with the given grid size.
func NewVTTerminal(cols, rows int) (*VTTerminal, error) {
	if cols <= 0 || rows <= 0 || cols > maxTerminalDimension || rows > maxTerminalDimension {
		return nil, fmt.Errorf("invalid terminal size %dx%d", cols, rows)
	}
	var terminal C.GhosttyTerminal
	if result := C.ghostty_terminal_new(
		nil,
		&terminal,
		C.uint16_t(cols),
		C.uint16_t(rows),
	); result != C.GHOSTTY_SUCCESS {
		return nil, fmt.Errorf("ghostty terminal new failed: %d", result)
	}
	return &VTTerminal{terminal: terminal}, nil
}

// Feed parses raw PTY bytes into the emulated terminal state.
func (v *VTTerminal) Feed(data []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil || len(data) == 0 {
		return
	}
	C.ghostty_terminal_vt_write(
		v.terminal,
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.size_t(len(data)),
	)
}

// Resize reflows the emulated terminal. The caller keeps the real PTY size
// in sync so snapshots are rendered at the client's dimensions.
func (v *VTTerminal) Resize(cols, rows int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil || cols <= 0 || rows <= 0 || cols > maxTerminalDimension || rows > maxTerminalDimension {
		return
	}
	_ = C.ghostty_terminal_resize(v.terminal, C.uint16_t(cols), C.uint16_t(rows), 8, 16)
}

// EncodeState encodes the full emulator state (visible grid, scrollback,
// cursor, and terminal modes) so a session can be migrated to another server
// process.
func (v *VTTerminal) EncodeState() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil {
		return nil, fmt.Errorf("ghostty terminal is closed")
	}
	var buffer *C.uint8_t
	var length C.size_t
	if result := C.ghostty_snapshot_encode_alloc(v.terminal, nil, &buffer, &length); result != C.GHOSTTY_SUCCESS {
		return nil, fmt.Errorf("ghostty snapshot encode failed: %d", result)
	}
	defer C.ghostty_free(nil, buffer, length)
	return C.GoBytes(unsafe.Pointer(buffer), C.int(length)), nil
}

// RestoreState replaces the emulated state with bytes produced by EncodeState.
func (v *VTTerminal) RestoreState(snapshot []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil {
		return fmt.Errorf("ghostty terminal is closed")
	}
	if len(snapshot) == 0 {
		return fmt.Errorf("ghostty snapshot is empty")
	}
	var decoder C.GhosttySnapshotDecoder
	if result := C.ghostty_snapshot_decoder_new_buf(
		nil,
		&decoder,
		(*C.uint8_t)(unsafe.Pointer(&snapshot[0])),
		C.size_t(len(snapshot)),
	); result != C.GHOSTTY_SUCCESS {
		return fmt.Errorf("ghostty snapshot decoder new failed: %d", result)
	}
	defer C.ghostty_snapshot_decoder_free(decoder)
	var restored C.GhosttyTerminal
	if result := C.ghostty_snapshot_decoder_decode(decoder, &restored); result != C.GHOSTTY_SUCCESS {
		return fmt.Errorf("ghostty snapshot decode failed: %d", result)
	}
	C.ghostty_terminal_free(v.terminal)
	v.terminal = restored
	return nil
}

// Snapshot renders the current emulated screen (visible grid + scrollback)
// as VT sequences that preserve colors and styles.
func (v *VTTerminal) Snapshot() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil {
		return nil, fmt.Errorf("ghostty terminal is closed")
	}
	var formatter C.GhosttyFormatter
	opts := C.GhosttyFormatterTerminalOptions{
		size: C.size_t(unsafe.Sizeof(C.GhosttyFormatterTerminalOptions{})),
		emit: C.GHOSTTY_FORMATTER_FORMAT_VT,
		trim: true,
	}
	if result := C.ghostty_formatter_terminal_new(nil, &formatter, v.terminal, opts); result != C.GHOSTTY_SUCCESS {
		return nil, fmt.Errorf("ghostty formatter new failed: %d", result)
	}
	defer C.ghostty_formatter_free(formatter)

	var buffer *C.uint8_t
	var length C.size_t
	if result := C.ghostty_formatter_format_alloc(formatter, nil, &buffer, &length); result != C.GHOSTTY_SUCCESS {
		return nil, fmt.Errorf("ghostty formatter format failed: %d", result)
	}
	defer C.ghostty_free(nil, buffer, length)
	return C.GoBytes(unsafe.Pointer(buffer), C.int(length)), nil
}

// Close releases the native terminal state. It must be called at most once.
func (v *VTTerminal) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal != nil {
		C.ghostty_terminal_free(v.terminal)
		v.terminal = nil
	}
}
