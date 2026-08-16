package ghostline

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Session is a stable handle to one session, local or remote.
type Session interface {
	// Name returns the session's unique name.
	Name() string
	// CreatedAt returns when the child process started.
	CreatedAt() time.Time
	// Done is closed after the child exits and all output is consumed.
	Done() <-chan struct{}
	// Wait waits for the child and returns its exit error. Context
	// cancellation stops waiting but does not terminate the child.
	Wait(ctx context.Context) error
	// Alive reports whether the session is currently running.
	Alive() bool
	// Status distinguishes a running session from a stopped one and reports
	// the exit reason when stopped.
	Status(ctx context.Context) (Status, error)
	// Input writes bytes to the PTY verbatim.
	Input(ctx context.Context, data []byte) error
	// Resize updates the real PTY and the emulated grid.
	Resize(ctx context.Context, size Size) error
	// Snapshot returns a full VT replay of the visible grid and scrollback.
	Snapshot(ctx context.Context) ([]byte, error)
	// Checkpoint captures a replay and its exact spool boundary atomically.
	Checkpoint(ctx context.Context) (Checkpoint, error)
	// Recover reads the raw spool range [offset, end).
	Recover(ctx context.Context, offset, end int64) ([]byte, error)
	// SpoolPath returns the append-only raw output spool path.
	SpoolPath() string
	// SpoolSize returns the current raw output spool size.
	SpoolSize(ctx context.Context) (int64, error)
	// WatchOutput subscribes to raw output and starts the watcher.
	WatchOutput(options WatchOptions) (*SpoolWatcher, error)
	// Close terminates the session. The record stays visible until Remove.
	Close() error
	// Remove deletes the session record. Spool files stay on disk.
	Remove() error
	// TruncateSpool compacts the live spool in place.
	TruncateSpool(ctx context.Context) error
	// ArchiveSpool compresses the spool and prunes old archives.
	ArchiveSpool(ctx context.Context) error
	// RemoveSpool deletes the spool and its archives.
	RemoveSpool()
}

// MetadataProvider is implemented by sessions that can report OS-level
// foreground process metadata. It is kept separate from Session so existing
// Session implementations and test doubles continue to compile.
type MetadataProvider interface {
	// Metadata reports OS-level foreground process metadata when the hub was
	// created with ProbeForeground enabled. When probing is disabled it
	// returns zero values; otherwise it may return an error when the
	// foreground process cannot be resolved.
	Metadata(ctx context.Context) (SessionMetadata, error)
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
	// Offset is the raw spool byte position covered by Replay.
	Offset int64
}

// WatchOptions configures an output subscription.
type WatchOptions struct {
	// Offset is the first spool byte to deliver.
	Offset int64
	// MaxBytes invokes OnOverflow after the watcher passes the limit. Zero
	// uses the watcher default.
	MaxBytes int64
	// OnOutput receives borrowed bytes; copy them to retain after return.
	OnOutput func([]byte)
	// OnTruncate runs when the spool is compacted in place.
	OnTruncate func()
	// OnOverflow runs when Offset passes MaxBytes.
	OnOverflow func()
}

// localSession runs a session inside the embedding process.
type localSession struct {
	hub   *Hub
	state *sessionState
}

func (l *localSession) Name() string          { return l.state.name }
func (l *localSession) CreatedAt() time.Time  { return l.state.createdAt }
func (l *localSession) Done() <-chan struct{} { return l.state.done }

func (l *localSession) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.state.done:
		l.state.waitMu.Lock()
		defer l.state.waitMu.Unlock()
		return convertExit(l.state.waitErr)
	}
}

func (l *localSession) Alive() bool {
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

func (l *localSession) Status(context.Context) (Status, error) {
	if l.Alive() {
		return Status{Alive: true}, nil
	}
	l.state.waitMu.Lock()
	defer l.state.waitMu.Unlock()
	exit, _ := convertExit(l.state.waitErr).(*ExitError)
	return Status{Exit: exit}, nil
}

func (l *localSession) Metadata(ctx context.Context) (SessionMetadata, error) {
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

func (l *localSession) Input(ctx context.Context, data []byte) error {
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
		if n < len(data) {
			log.Printf("ghostline: short pty write %d/%d, retrying", n, len(data))
		}
		data = data[n:]
	}
	return nil
}

func (l *localSession) Resize(ctx context.Context, size Size) error {
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

func (l *localSession) Snapshot(ctx context.Context) ([]byte, error) {
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

func (l *localSession) Checkpoint(ctx context.Context) (Checkpoint, error) {
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
	info, err := os.Stat(l.state.path)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("stat spool: %w", err)
	}
	return Checkpoint{Replay: replay, Offset: info.Size()}, nil
}

func (l *localSession) Recover(ctx context.Context, offset, end int64) ([]byte, error) {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return readSpool(l.state.path, offset, end)
}

func (l *localSession) SpoolPath() string { return l.state.path }

func (l *localSession) SpoolSize(ctx context.Context) (int64, error) {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return spoolSize(l.state.path)
}

func (l *localSession) WatchOutput(options WatchOptions) (*SpoolWatcher, error) {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if !l.current() {
		return nil, ErrSessionClosed
	}
	watcher, err := NewSpoolWatcher(
		l.state.path,
		options.Offset,
		options.OnOutput,
		options.OnTruncate,
		options.OnOverflow,
	)
	if err != nil {
		return nil, err
	}
	watcher.SetMaxBytes(options.MaxBytes)
	l.state.watcherMu.Lock()
	l.state.watchers = append(l.state.watchers, watcher)
	l.state.watcherMu.Unlock()
	watcher.Start()
	return watcher, nil
}

func (l *localSession) Close() error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if !l.current() {
		return nil
	}
	return terminate(l.state)
}

func (l *localSession) Remove() error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if !l.current() {
		return nil
	}
	l.hub.remove(l.state.name)
	return terminate(l.state)
}

func (l *localSession) TruncateSpool(ctx context.Context) error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	return truncateSpool(l.state.path)
}

func (l *localSession) ArchiveSpool(ctx context.Context) error {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	return archiveSpool(l.state.path)
}

func (l *localSession) RemoveSpool() {
	l.hub.lifecycleMu.RLock()
	defer l.hub.lifecycleMu.RUnlock()
	l.state.operationMu.RLock()
	defer l.state.operationMu.RUnlock()
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	removeSpool(l.state.path)
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

func readSpool(path string, offset, end int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open spool: %w", err)
	}
	defer closeQuietly(file)
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > info.Size() {
		return nil, fmt.Errorf("spool offset %d beyond size %d", offset, info.Size())
	}
	if end > info.Size() {
		end = info.Size()
	}
	if end < offset {
		end = offset
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data := make([]byte, end-offset)
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, err
	}
	return data, nil
}

func spoolSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func truncateSpool(path string) error {
	if err := os.Truncate(path, 0); err != nil {
		return fmt.Errorf("truncate spool: %w", err)
	}
	return nil
}

func archiveSpool(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return nil
	}
	archive := path + "." + strconv.FormatInt(time.Now().UnixNano(), 10) + ".gz"
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer closeQuietly(source)
	destination, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := gzip.NewWriter(destination)
	if _, err := io.Copy(writer, source); err != nil {
		_ = writer.Close()
		_ = destination.Close()
		removeQuietly(archive)
		return err
	}
	if err := writer.Close(); err != nil {
		_ = destination.Close()
		removeQuietly(archive)
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	return pruneArchives(path)
}

func pruneArchives(path string) error {
	matches, err := filepath.Glob(path + ".*.gz")
	if err != nil {
		return err
	}
	for len(matches) > 3 {
		oldest := 0
		for index := 1; index < len(matches); index++ {
			if matches[index] < matches[oldest] {
				oldest = index
			}
		}
		removeQuietly(matches[oldest])
		matches = append(matches[:oldest], matches[oldest+1:]...)
	}
	return nil
}

func removeSpool(path string) {
	removeQuietly(path)
	matches, err := filepath.Glob(path + ".*.gz")
	if err != nil {
		return
	}
	for _, match := range matches {
		removeQuietly(match)
	}
}
