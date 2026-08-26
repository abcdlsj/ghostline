package ghostline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Session is a concrete handle to one local or daemon-owned terminal session.
// Immutable identity is cached in the handle. Every method that can perform
// process, storage, or network I/O accepts a context and returns an error.
type Session struct {
	backend   sessionBackend
	name      string
	createdAt time.Time
}

// SessionInfo is immutable session identity returned by List.
type SessionInfo struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// Name returns the session's unique name without performing I/O.
func (s *Session) Name() string { return s.name }

// CreatedAt returns when the child process started without performing I/O.
func (s *Session) CreatedAt() time.Time { return s.createdAt }

// Info returns the session's immutable identity without performing I/O.
func (s *Session) Info() SessionInfo {
	return SessionInfo{Name: s.name, CreatedAt: s.createdAt}
}

// Wait waits for the child and returns its exit error. A fatal output storage
// error is joined with the exit error. Canceling ctx stops waiting but does
// not terminate the child.
func (s *Session) Wait(ctx context.Context) error { return s.backend.wait(ctx) }

// Status reports whether the session is running and, when stopped, why. A
// fatal session runtime or storage failure is returned as an error.
func (s *Session) Status(ctx context.Context) (Status, error) {
	return s.backend.status(ctx)
}

// Metadata reports the foreground process metadata when probing is enabled.
func (s *Session) Metadata(ctx context.Context) (SessionMetadata, error) {
	return s.backend.metadata(ctx)
}

// Size returns the current PTY grid size.
func (s *Session) Size(ctx context.Context) (Size, error) {
	return s.backend.size(ctx)
}

// Signal sends signal to the session's process group. signal must be a
// non-zero syscall.Signal, such as os.Interrupt or syscall.SIGTERM.
func (s *Session) Signal(ctx context.Context, signal os.Signal) error {
	native, err := normalizeSignal(signal)
	if err != nil {
		return err
	}
	return s.backend.signal(ctx, native)
}

// WriteInput writes bytes to the PTY verbatim.
func (s *Session) WriteInput(ctx context.Context, data []byte) error {
	return s.backend.writeInput(ctx, data)
}

// Resize updates the real PTY and the emulated grid.
func (s *Session) Resize(ctx context.Context, size Size) error {
	return s.backend.resize(ctx, size)
}

// Replay renders the visible grid and scrollback as terminal bytes.
func (s *Session) Replay(ctx context.Context) ([]byte, error) {
	return s.backend.replay(ctx)
}

// Checkpoint atomically captures a replay and its output position.
func (s *Session) Checkpoint(ctx context.Context) (Checkpoint, error) {
	return s.backend.checkpoint(ctx)
}

// Output streams raw PTY output beginning at from. The zero Cursor starts at
// the earliest retained byte. The caller must close the returned reader.
func (s *Session) Output(ctx context.Context, from Cursor) (*OutputReader, error) {
	return s.backend.output(ctx, from)
}

// OutputCursor returns the current end of retained raw output without
// capturing a VT replay. Bytes may be appended immediately after it returns.
// Use Checkpoint when the cursor must be atomically paired with a replay.
func (s *Session) OutputCursor(ctx context.Context) (Cursor, error) {
	return s.backend.outputCursor(ctx)
}

// RotateOutput completes the active output segment and returns the boundary
// cursor at the beginning of the new generation.
func (s *Session) RotateOutput(ctx context.Context) (Cursor, error) {
	return s.backend.rotateOutput(ctx)
}

// PruneOutput removes immutable generations strictly before before. before
// must be a generation-boundary cursor returned by RotateOutput.
func (s *Session) PruneOutput(ctx context.Context, before Cursor) error {
	return s.backend.pruneOutput(ctx, before)
}

// Terminate ends the process tree but keeps the session record and output.
func (s *Session) Terminate(ctx context.Context) error {
	return s.backend.terminate(ctx)
}

// Delete ends the process tree and removes the session record and output
// storage. Callers that need retained output must archive it before Delete.
func (s *Session) Delete(ctx context.Context) error {
	return s.backend.delete(ctx)
}

func newSession(backend sessionBackend, info SessionInfo) *Session {
	return &Session{backend: backend, name: info.Name, createdAt: info.CreatedAt}
}

// sessionBackend is implemented by the in-process and daemon transports. It
// stays private so adding implementation capabilities cannot break users or
// their test doubles.
type sessionBackend interface {
	wait(context.Context) error
	status(context.Context) (Status, error)
	metadata(context.Context) (SessionMetadata, error)
	size(context.Context) (Size, error)
	signal(context.Context, syscall.Signal) error
	writeInput(context.Context, []byte) error
	resize(context.Context, Size) error
	replay(context.Context) ([]byte, error)
	checkpoint(context.Context) (Checkpoint, error)
	output(context.Context, Cursor) (*OutputReader, error)
	outputCursor(context.Context) (Cursor, error)
	rotateOutput(context.Context) (Cursor, error)
	pruneOutput(context.Context, Cursor) error
	terminate(context.Context) error
	delete(context.Context) error
}

func normalizeSignal(signal os.Signal) (syscall.Signal, error) {
	if signal == nil {
		return 0, ErrInvalidSignal
	}
	native, ok := signal.(syscall.Signal)
	if !ok || native <= 0 {
		return 0, fmt.Errorf("%w: %T", ErrInvalidSignal, signal)
	}
	return native, nil
}

// Status describes whether a session is running and, when stopped, why.
type Status struct {
	// Alive is true while the child process is running.
	Alive bool `json:"alive"`
	// Exit describes the termination when Alive is false.
	Exit *ExitError `json:"exit,omitempty"`
}

// SessionMetadata is the OS-level foreground process snapshot for one
// session. It is presentation metadata, not lifecycle state.
type SessionMetadata struct {
	// Process is the foreground process name.
	Process string `json:"process"`
	// Directory is the foreground process working directory.
	Directory string `json:"directory"`
}

// Checkpoint is an atomic screen replay and raw output position.
type Checkpoint struct {
	// Replay is a full VT replay of the visible grid and scrollback.
	Replay []byte
	// Cursor is the first raw output byte not covered by Replay.
	Cursor Cursor
}

// localSession runs a session inside the embedding process.
type localSession struct {
	hub   *Hub
	state *sessionState
}

func (l *localSession) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.state.done:
		outputErr := l.state.output.terminalError()
		l.state.waitMu.Lock()
		defer l.state.waitMu.Unlock()
		return errors.Join(outputErr, convertExit(l.state.waitErr))
	}
}

func (l *localSession) alive() bool {
	if !l.current() {
		return false
	}
	select {
	case <-l.state.done:
		return false
	default:
		return true
	}
}

func (l *localSession) status(ctx context.Context) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	if err := l.state.output.terminalError(); err != nil {
		return Status{}, err
	}
	if l.alive() {
		return Status{Alive: true}, nil
	}
	l.state.waitMu.Lock()
	defer l.state.waitMu.Unlock()
	exit, _ := convertExit(l.state.waitErr).(*ExitError)
	return Status{Exit: exit}, nil
}

func (l *localSession) metadata(ctx context.Context) (SessionMetadata, error) {
	if !l.hub.probeForeground {
		return SessionMetadata{}, nil
	}
	if err := ctx.Err(); err != nil {
		return SessionMetadata{}, err
	}
	l.hub.lifecycleMu.RLock()
	l.state.operationMu.RLock()
	if l.state.masterFD < 0 {
		l.state.operationMu.RUnlock()
		l.hub.lifecycleMu.RUnlock()
		return SessionMetadata{}, nil
	}
	fd, err := duplicateMasterFD(l.state.masterFD)
	l.state.operationMu.RUnlock()
	l.hub.lifecycleMu.RUnlock()
	if err != nil {
		return SessionMetadata{}, nil
	}
	defer func() { _ = closeMasterFD(fd) }()
	return probeForegroundFD(ctx, fd)
}

func (l *localSession) size(ctx context.Context) (Size, error) {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Size{}, err
	}
	if !l.current() {
		return Size{}, ErrSessionClosed
	}
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	return l.state.size, nil
}

func (l *localSession) signal(ctx context.Context, signal syscall.Signal) error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.current() {
		return ErrSessionClosed
	}
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	select {
	case <-l.state.done:
		return os.ErrProcessDone
	default:
	}
	if l.state.pid <= 0 {
		return os.ErrProcessDone
	}
	// pty.StartWithSize creates a new session, but a shell or wrapper is free
	// to change its process group before the session is signalled. Resolve the
	// live group instead of assuming it is always equal to the original PID;
	// that assumption is not stable across Unix runners and can break signals
	// after PTY handoff on some Linux images.
	pgid, err := unix.Getpgid(l.state.pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return fmt.Errorf("get session process group: %w", err)
	}
	if pgid <= 0 {
		return os.ErrProcessDone
	}
	if err := unix.Kill(-pgid, signal); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return fmt.Errorf("signal session process group: %w", err)
	}
	return nil
}

func (l *localSession) writeInput(ctx context.Context, data []byte) error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.current() {
		return ErrSessionClosed
	}
	if len(data) == 0 {
		return nil
	}
	l.state.inputMu.Lock()
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	defer l.state.inputMu.Unlock()
	if err := writeFull(l.state.master, data); err != nil {
		return fmt.Errorf("write pty input: %w", err)
	}
	return nil
}

// writeFull writes data to w, retrying short writes so the PTY never
// silently drops the tail of an input frame (for example the closing bytes
// of a bracketed-paste sequence). A zero-progress write is treated as
// io.ErrShortWrite instead of spinning forever.
func writeFull(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (l *localSession) resize(ctx context.Context, size Size) error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := size.resolve(Size{}); err != nil {
		return err
	}
	if !l.current() {
		return ErrSessionClosed
	}
	l.state.inputMu.Lock()
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	defer l.state.inputMu.Unlock()
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	// Resize the server-side emulator before the real PTY. The child sees
	// SIGWINCH only after ptySetSize and redraws immediately; the emulator
	// must already be at the new size or that redraw is parsed against the
	// old grid, leaving cursor and input-box positions wrong.
	l.state.vt.Resize(size.Columns, size.Rows)
	if err := ptySetSize(l.state.master, size); err != nil {
		return err
	}
	l.state.size = size
	return nil
}

func (l *localSession) replay(ctx context.Context) ([]byte, error) {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !l.current() {
		return nil, ErrSessionClosed
	}
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	return captureLocked(l.state)
}

func (l *localSession) checkpoint(ctx context.Context) (Checkpoint, error) {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Checkpoint{}, err
	}
	if !l.current() {
		return Checkpoint{}, ErrSessionClosed
	}
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	replay, err := captureLocked(l.state)
	if err != nil {
		return Checkpoint{}, err
	}
	cursor, err := l.state.output.cursor()
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{Replay: replay, Cursor: cursor}, nil
}

func (l *localSession) output(ctx context.Context, from Cursor) (*OutputReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return l.state.output.reader(ctx, from)
}

func (l *localSession) outputCursor(ctx context.Context) (Cursor, error) {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Cursor{}, err
	}
	if !l.current() {
		return Cursor{}, ErrSessionClosed
	}
	return l.state.output.cursor()
}

func (l *localSession) rotateOutput(ctx context.Context) (Cursor, error) {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return Cursor{}, err
	}
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	return l.state.output.rotate()
}

func (l *localSession) pruneOutput(ctx context.Context, before Cursor) error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return l.state.output.prune(before)
}

func (l *localSession) terminate(ctx context.Context) error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.current() {
		return nil
	}
	return terminate(l.state)
}

func (l *localSession) delete(ctx context.Context) error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.current() {
		return nil
	}
	l.hub.remove(l.state.name)
	err := terminate(l.state)
	l.state.output.discard()
	return err
}

func (l *localSession) current() bool {
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	return l.hub.sessions[l.state.name] == l.state
}

func captureLocked(state *sessionState) ([]byte, error) {
	snapshot, err := state.vt.Snapshot()
	if err != nil {
		return nil, err
	}
	result := make([]byte, 0, len(snapshot)+7)
	result = append(result, "\x1b[3J\x1b[2J\x1b[H"...)
	result = append(result, snapshot...)
	return result, nil
}
