//go:build cgo && ((darwin && (amd64 || arm64)) || (linux && (amd64 || arm64)))

package ghostline

/*
#cgo CFLAGS: -I${SRCDIR}/third_party/include
#cgo darwin LDFLAGS: ${SRCDIR}/third_party/lib/libghostty-vt.a -lc++
#cgo linux,amd64 LDFLAGS: ${SRCDIR}/third_party/lib/linux_amd64/libghostty-vt.a -lstdc++ -lm -lrt
#cgo linux,arm64 LDFLAGS: ${SRCDIR}/third_party/lib/linux_arm64/libghostty-vt.a -lstdc++ -lm -lrt
#cgo !darwin,!linux LDFLAGS: -lghostty-vt
#include <stdlib.h>
#include <ghostty/vt.h>
#include <ghostty/vt/snapshot.h>

static bool ghostline_terminal_cell_is_wide(
    GhosttyTerminal terminal,
    GhosttyPointTag tag,
    uint16_t x,
    uint32_t y
) {
    GhosttyPoint point = {0};
    point.tag = tag;
    point.value.coordinate.x = x;
    point.value.coordinate.y = y;

    GhosttyGridRef ref = {0};
    ref.size = sizeof(ref);
    if (ghostty_terminal_grid_ref(terminal, point, &ref) != GHOSTTY_SUCCESS) {
        return false;
    }

    GhosttyCell cell = 0;
    if (ghostty_grid_ref_cell(&ref, &cell) != GHOSTTY_SUCCESS) {
        return false;
    }

    GhosttyCellWide wide = GHOSTTY_CELL_WIDE_NARROW;
    if (ghostty_cell_get(cell, GHOSTTY_CELL_DATA_WIDE, &wide) != GHOSTTY_SUCCESS) {
        return false;
    }
    return wide == GHOSTTY_CELL_WIDE_WIDE;
}

static bool ghostline_terminal_is_ground(GhosttyTerminal terminal) {
    bool ground = false;
    return ghostty_terminal_get(
        terminal,
        GHOSTTY_TERMINAL_DATA_VT_GROUND,
        &ground
    ) == GHOSTTY_SUCCESS && ground;
}

static bool ghostline_terminal_mode(
    GhosttyTerminal terminal,
    GhosttyMode mode
) {
    GhosttyTerminalModeConfig config = {
        .mode = mode,
        .value = false,
    };
    return ghostty_terminal_get(
        terminal,
        GHOSTTY_TERMINAL_DATA_MODE,
        &config
    ) == GHOSTTY_SUCCESS && config.value;
}
*/
import "C"

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"sync"
	"unsafe"
)

// snapshotContinuationMaxBytes keeps unfinished VT and UTF-8 input replayable
// during migration. It is independent of the resize repair below.
const snapshotContinuationMaxBytes = 1 << 20

const (
	vtStateMagic      = "ghostline-vt-v1\x00"
	vtStateHeaderSize = len(vtStateMagic) + 8 + sha256.Size
)

type vtTerminal struct {
	mu                 sync.Mutex
	terminal           C.GhosttyTerminal
	scrollbackMaxBytes uint64
}

func newVTTerminal(cols, rows int) (*vtTerminal, error) {
	return newVTTerminalWithOptions(cols, rows, vtTerminalOptions{})
}

func newVTTerminalWithOptions(cols, rows int, options vtTerminalOptions) (*vtTerminal, error) {
	if cols <= 0 || rows <= 0 || cols > maxTerminalDimension || rows > maxTerminalDimension {
		return nil, fmt.Errorf("invalid terminal size %dx%d", cols, rows)
	}
	scrollbackMaxBytes := options.resolvedScrollbackMaxBytes()
	var terminal C.GhosttyTerminal
	if result := C.ghostty_terminal_new(
		nil,
		&terminal,
		C.uint16_t(cols),
		C.uint16_t(rows),
	); result != C.GHOSTTY_SUCCESS {
		return nil, fmt.Errorf("ghostty terminal new failed: %d", result)
	}
	if err := setScrollbackMaxBytes(terminal, scrollbackMaxBytes); err != nil {
		C.ghostty_terminal_free(terminal)
		return nil, err
	}
	if err := enableContinuationTracking(terminal); err != nil {
		C.ghostty_terminal_free(terminal)
		return nil, err
	}
	return &vtTerminal{
		terminal:           terminal,
		scrollbackMaxBytes: scrollbackMaxBytes,
	}, nil
}

func setScrollbackMaxBytes(terminal C.GhosttyTerminal, maxBytes uint64) error {
	value := C.size_t(maxBytes)
	if uint64(value) != maxBytes {
		return fmt.Errorf("scrollback limit exceeds platform size_t: %d", maxBytes)
	}
	if result := C.ghostty_terminal_set(
		terminal,
		C.GHOSTTY_TERMINAL_OPT_SCROLLBACK_MAX_BYTES,
		unsafe.Pointer(&value),
	); result != C.GHOSTTY_SUCCESS {
		return fmt.Errorf("configure scrollback: %d", result)
	}
	return nil
}

func enableContinuationTracking(terminal C.GhosttyTerminal) error {
	maxContinuation := C.size_t(snapshotContinuationMaxBytes)
	if result := C.ghostty_terminal_set(
		terminal,
		C.GHOSTTY_TERMINAL_OPT_CONTINUATION_MAX_BYTES,
		unsafe.Pointer(&maxContinuation),
	); result != C.GHOSTTY_SUCCESS {
		return fmt.Errorf("enable continuation tracking: %d", result)
	}
	return nil
}

// Feed parses raw PTY bytes into the emulated terminal state.
func (v *vtTerminal) Feed(data []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil || len(data) == 0 {
		return
	}
	v.writeLocked(data)
}

func (v *vtTerminal) writeLocked(data []byte) {
	if len(data) == 0 {
		return
	}
	C.ghostty_terminal_vt_write(
		v.terminal,
		(*C.uint8_t)(unsafe.Pointer(&data[0])),
		C.size_t(len(data)),
	)
}

// clearWideResizeBoundaryLocked removes wide heads that would be left without
// their spacer tail by an old no-reflow resize implementation. The public
// Ghostty API exposes cells as read-only, so the repair is expressed as an
// ordinary erase operation on the active terminal screen.
func (v *vtTerminal) clearWideResizeBoundaryLocked(cols, rows int, onlyNoReflow bool) {
	if v.terminal == nil || cols <= 0 || rows <= 0 {
		return
	}

	var currentCols C.uint16_t
	var currentRows C.uint16_t
	if C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_COLS,
		unsafe.Pointer(&currentCols),
	) != C.GHOSTTY_SUCCESS || C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_ROWS,
		unsafe.Pointer(&currentRows),
	) != C.GHOSTTY_SUCCESS {
		return
	}
	if cols > int(currentCols) {
		return
	}

	if onlyNoReflow {
		var screen C.GhosttyTerminalScreen
		if C.ghostty_terminal_get(
			v.terminal,
			C.GHOSTTY_TERMINAL_DATA_ACTIVE_SCREEN,
			unsafe.Pointer(&screen),
		) != C.GHOSTTY_SUCCESS {
			return
		}
		if screen != C.GHOSTTY_TERMINAL_SCREEN_ALTERNATE &&
			C.ghostline_terminal_mode(v.terminal, C.GHOSTTY_MODE_WRAPAROUND) {
			return
		}
	}

	// VT cursor addressing is unsafe while an escape sequence is incomplete.
	// Origin mode changes the meaning of CUP and has no public margin query, so
	// leave that uncommon state untouched rather than moving the wrong cells.
	if !C.ghostline_terminal_is_ground(v.terminal) ||
		C.ghostline_terminal_mode(v.terminal, C.GHOSTTY_MODE_ORIGIN) {
		return
	}

	var cursorX C.uint16_t
	var cursorY C.uint16_t
	if C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_CURSOR_X,
		unsafe.Pointer(&cursorX),
	) != C.GHOSTTY_SUCCESS || C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_CURSOR_Y,
		unsafe.Pointer(&cursorY),
	) != C.GHOSTTY_SUCCESS {
		return
	}

	boundary := C.uint16_t(cols - 1)
	rowLimit := int(currentRows)
	if rows < rowLimit {
		rowLimit = rows
	}
	var erase []byte
	for row := 0; row < rowLimit; row++ {
		if !C.ghostline_terminal_cell_is_wide(
			v.terminal,
			C.GHOSTTY_POINT_TAG_ACTIVE,
			boundary,
			C.uint32_t(row),
		) {
			continue
		}
		erase = append(erase, '\x1b', '[')
		erase = strconv.AppendInt(erase, int64(row+1), 10)
		erase = append(erase, ';')
		erase = strconv.AppendInt(erase, int64(cols), 10)
		erase = append(erase, 'H', '\x1b', '[', '1', 'X')
	}
	if len(erase) > 0 {
		erase = append(erase, '\x1b', '[')
		erase = strconv.AppendInt(erase, int64(cursorY)+1, 10)
		erase = append(erase, ';')
		erase = strconv.AppendInt(erase, int64(cursorX)+1, 10)
		erase = append(erase, 'H')
	}
	v.writeLocked(erase)
}

// clearInactiveAlternateBoundaryLocked briefly visits the alternate screen so
// an older no-reflow resize cannot leave an unpaired wide head there. This is
// only used after snapshot encoding reports GHOSTTY_INVALID_VALUE; the public
// API has no read-only handle for an inactive screen.
func (v *vtTerminal) clearInactiveAlternateBoundaryLocked() {
	if v.terminal == nil || !C.ghostline_terminal_is_ground(v.terminal) ||
		C.ghostline_terminal_mode(v.terminal, C.GHOSTTY_MODE_ORIGIN) {
		return
	}

	var screen C.GhosttyTerminalScreen
	if C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_ACTIVE_SCREEN,
		unsafe.Pointer(&screen),
	) != C.GHOSTTY_SUCCESS {
		return
	}

	enterAlternate := []byte("\x1b[?47h")
	leaveAlternate := []byte("\x1b[?47l")
	if screen == C.GHOSTTY_TERMINAL_SCREEN_ALTERNATE {
		enterAlternate, leaveAlternate = leaveAlternate, enterAlternate
	}
	// Mode 47 copies the cursor when switching screens. Restore the active
	// position explicitly while leaving the screen-local saved cursor intact.
	var cursorX C.uint16_t
	var cursorY C.uint16_t
	if C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_CURSOR_X,
		unsafe.Pointer(&cursorX),
	) != C.GHOSTTY_SUCCESS || C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_CURSOR_Y,
		unsafe.Pointer(&cursorY),
	) != C.GHOSTTY_SUCCESS {
		return
	}
	v.writeLocked(enterAlternate)
	var cols C.uint16_t
	var rows C.uint16_t
	if C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_COLS,
		unsafe.Pointer(&cols),
	) == C.GHOSTTY_SUCCESS && C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_ROWS,
		unsafe.Pointer(&rows),
	) == C.GHOSTTY_SUCCESS {
		v.clearWideResizeBoundaryLocked(int(cols), int(rows), false)
	}
	v.writeLocked(leaveAlternate)
	restore := []byte{'\x1b', '['}
	restore = strconv.AppendInt(restore, int64(cursorY)+1, 10)
	restore = append(restore, ';')
	restore = strconv.AppendInt(restore, int64(cursorX)+1, 10)
	restore = append(restore, 'H')
	v.writeLocked(restore)
}

func (v *vtTerminal) encodeStateLocked() ([]byte, C.GhosttyResult) {
	var buffer *C.uint8_t
	var length C.size_t
	result := C.ghostty_snapshot_encode_alloc(v.terminal, nil, &buffer, &length)
	if result != C.GHOSTTY_SUCCESS {
		return nil, result
	}
	defer C.ghostty_free(nil, buffer, length)
	return C.GoBytes(unsafe.Pointer(buffer), C.int(length)), result
}

// Resize reflows the emulated terminal. The caller keeps the real PTY size
// in sync so snapshots are rendered at the client's dimensions.
func (v *vtTerminal) Resize(cols, rows int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil || cols <= 0 || rows <= 0 || cols > maxTerminalDimension || rows > maxTerminalDimension {
		return
	}
	v.clearWideResizeBoundaryLocked(cols, rows, true)
	_ = C.ghostty_terminal_resize(v.terminal, C.uint16_t(cols), C.uint16_t(rows), 8, 16)
}

// EncodeState encodes the full emulator state (visible grid, scrollback,
// cursor, and terminal modes) so a session can be migrated to another server
// process.
func (v *vtTerminal) EncodeState() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil {
		return nil, fmt.Errorf("ghostty terminal is closed")
	}
	// Repair an already-resized active screen as a fallback for terminals
	// created by an older Ghostty build.
	var currentCols C.uint16_t
	var currentRows C.uint16_t
	if C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_COLS,
		unsafe.Pointer(&currentCols),
	) == C.GHOSTTY_SUCCESS && C.ghostty_terminal_get(
		v.terminal,
		C.GHOSTTY_TERMINAL_DATA_ROWS,
		unsafe.Pointer(&currentRows),
	) == C.GHOSTTY_SUCCESS {
		v.clearWideResizeBoundaryLocked(int(currentCols), int(currentRows), false)
	}
	snapshot, result := v.encodeStateLocked()
	if result == C.GHOSTTY_INVALID_VALUE {
		v.clearInactiveAlternateBoundaryLocked()
		snapshot, result = v.encodeStateLocked()
	}
	if result != C.GHOSTTY_SUCCESS {
		return nil, fmt.Errorf("ghostty snapshot encode failed: %d", result)
	}
	return wrapVTState(snapshot), nil
}

func (v *vtTerminal) RestoreState(snapshot []byte) error {
	raw, err := unwrapVTState(snapshot)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil {
		return fmt.Errorf("ghostty terminal is closed")
	}
	var decoder C.GhosttySnapshotDecoder
	if result := C.ghostty_snapshot_decoder_new_buf(
		nil,
		&decoder,
		(*C.uint8_t)(unsafe.Pointer(&raw[0])),
		C.size_t(len(raw)),
	); result != C.GHOSTTY_SUCCESS {
		return fmt.Errorf("ghostty snapshot decoder new failed: %d", result)
	}
	defer C.ghostty_snapshot_decoder_free(decoder)
	var restored C.GhosttyTerminal
	if result := C.ghostty_snapshot_decoder_decode(decoder, &restored); result != C.GHOSTTY_SUCCESS {
		return fmt.Errorf("ghostty snapshot decode failed: %d", result)
	}
	if err := setScrollbackMaxBytes(restored, v.scrollbackMaxBytes); err != nil {
		C.ghostty_terminal_free(restored)
		return err
	}
	if err := enableContinuationTracking(restored); err != nil {
		C.ghostty_terminal_free(restored)
		return err
	}
	C.ghostty_terminal_free(v.terminal)
	v.terminal = restored
	return nil
}

func wrapVTState(raw []byte) []byte {
	state := make([]byte, vtStateHeaderSize+len(raw))
	copy(state, vtStateMagic)
	binary.LittleEndian.PutUint64(state[len(vtStateMagic):], uint64(len(raw)))
	digest := sha256.Sum256(raw)
	copy(state[len(vtStateMagic)+8:], digest[:])
	copy(state[vtStateHeaderSize:], raw)
	return state
}

func unwrapVTState(state []byte) ([]byte, error) {
	if len(state) < vtStateHeaderSize || !bytes.Equal(state[:len(vtStateMagic)], []byte(vtStateMagic)) {
		return nil, fmt.Errorf("invalid ghostline VT state envelope")
	}
	raw := state[vtStateHeaderSize:]
	if binary.LittleEndian.Uint64(state[len(vtStateMagic):]) != uint64(len(raw)) {
		return nil, fmt.Errorf("invalid ghostline VT state length")
	}
	want := state[len(vtStateMagic)+8 : vtStateHeaderSize]
	got := sha256.Sum256(raw)
	if !bytes.Equal(want, got[:]) {
		return nil, fmt.Errorf("invalid ghostline VT state checksum")
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("ghostline VT state payload is empty")
	}
	return raw, nil
}

// Snapshot renders the current emulated screen (visible grid + scrollback)
// as VT sequences that preserve colors and styles.
func (v *vtTerminal) Snapshot() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal == nil {
		return nil, fmt.Errorf("ghostty terminal is closed")
	}
	var formatter C.GhosttyFormatter
	opts := C.GhosttyFormatterTerminalOptions{
		size: C.size_t(unsafe.Sizeof(C.GhosttyFormatterTerminalOptions{})),
		emit: C.GHOSTTY_FORMATTER_FORMAT_VT,
		// Preserve trailing blank cells and their SGR attributes. Full-screen
		// TUIs commonly paint composer backgrounds across otherwise empty rows;
		// trimming those cells collapses the coloured block on recovery until a
		// subsequent resize forces the application to repaint.
		trim: false,
	}
	// The VT formatter describes screen content but not the final cursor
	// position. Replaying a snapshot without CUP leaves the client's cursor
	// after the last emitted row (often the status line) instead of at the
	// application's input box, which breaks TUIs such as Codex.
	opts.extra.screen.cursor = true
	opts.extra.modes = true
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
func (v *vtTerminal) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.terminal != nil {
		C.ghostty_terminal_free(v.terminal)
		v.terminal = nil
	}
}
