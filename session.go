package ghostline

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type sessionBackend interface {
	Name() string
	CreatedAt() time.Time
	Done() <-chan struct{}
	Wait(ctx context.Context) error
	Alive() bool
	Status(ctx context.Context) (bool, error)
	Input(ctx context.Context, data []byte) error
	Resize(ctx context.Context, size Size) error
	Snapshot(ctx context.Context) ([]byte, error)
	Checkpoint(ctx context.Context) (Checkpoint, error)
	Recover(ctx context.Context, offset, end int64) ([]byte, error)
	SpoolPath() string
	SpoolSize(ctx context.Context) (int64, error)
	WatchOutput(options WatchOptions) (*SpoolWatcher, error)
	Close() error
	Remove() error
	TruncateSpool(ctx context.Context) error
	ArchiveSpool(ctx context.Context) error
	RemoveSpool()
}

// Session is a stable handle to one session, local or remote.
type Session struct {
	backend sessionBackend
}

// Name returns the session's unique name.
func (s *Session) Name() string { return s.backend.Name() }

// CreatedAt returns when the child process started.
func (s *Session) CreatedAt() time.Time { return s.backend.CreatedAt() }

// Done is closed after the child exits and all output has been consumed.
func (s *Session) Done() <-chan struct{} { return s.backend.Done() }

// Wait waits for the child and returns its exit error. Context cancellation
// stops waiting but does not terminate the child.
func (s *Session) Wait(ctx context.Context) error { return s.backend.Wait(ctx) }

// Alive reports whether the session is currently running.
func (s *Session) Alive() bool { return s.backend.Alive() }

// Status reports whether the session is running, distinguishing a remote
// network failure from a stopped session.
func (s *Session) Status(ctx context.Context) (bool, error) {
	return s.backend.Status(ctx)
}

// Input writes bytes to the PTY verbatim.
func (s *Session) Input(ctx context.Context, data []byte) error {
	return s.backend.Input(ctx, data)
}

// Resize updates the real PTY and the emulated grid.
func (s *Session) Resize(ctx context.Context, size Size) error {
	return s.backend.Resize(ctx, size)
}

// Snapshot returns a full VT replay of the visible grid and scrollback.
func (s *Session) Snapshot(ctx context.Context) ([]byte, error) {
	return s.backend.Snapshot(ctx)
}

// Checkpoint captures a replay and its exact spool boundary atomically.
func (s *Session) Checkpoint(ctx context.Context) (Checkpoint, error) {
	return s.backend.Checkpoint(ctx)
}

// Recover reads the raw spool range [offset, end).
func (s *Session) Recover(ctx context.Context, offset, end int64) ([]byte, error) {
	return s.backend.Recover(ctx, offset, end)
}

// SpoolPath returns the append-only raw output spool path.
func (s *Session) SpoolPath() string { return s.backend.SpoolPath() }

// SpoolSize returns the current raw output spool size.
func (s *Session) SpoolSize(ctx context.Context) (int64, error) {
	return s.backend.SpoolSize(ctx)
}

// WatchOutput subscribes to raw output and starts the watcher before returning.
func (s *Session) WatchOutput(options WatchOptions) (*SpoolWatcher, error) {
	return s.backend.WatchOutput(options)
}

// Close terminates the session. The record stays visible until Remove.
func (s *Session) Close() error { return s.backend.Close() }

// Remove deletes the session record. Spool files stay on disk.
func (s *Session) Remove() error { return s.backend.Remove() }

// TruncateSpool compacts the live spool in place.
func (s *Session) TruncateSpool(ctx context.Context) error {
	return s.backend.TruncateSpool(ctx)
}

// ArchiveSpool compresses the spool to a timestamped .gz file and prunes old
// archives. Best-effort diagnostics; truncation must not depend on it.
func (s *Session) ArchiveSpool(ctx context.Context) error {
	return s.backend.ArchiveSpool(ctx)
}

// RemoveSpool deletes the spool and its archives.
func (s *Session) RemoveSpool() { s.backend.RemoveSpool() }

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

func (l *localSession) Status(context.Context) (bool, error) {
	return l.Alive(), nil
}

func (l *localSession) Input(ctx context.Context, data []byte) error {
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
	defer l.state.inputMu.Unlock()
	if _, err := l.state.master.Write(data); err != nil {
		return fmt.Errorf("write pty input: %w", err)
	}
	return nil
}

func (l *localSession) Resize(ctx context.Context, size Size) error {
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
	defer l.state.inputMu.Unlock()
	if err := ptySetSize(l.state.master, size); err != nil {
		return err
	}
	l.state.vt.Resize(size.Columns, size.Rows)
	return nil
}

func (l *localSession) Snapshot(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !l.current() {
		return nil, ErrSessionClosed
	}
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	return captureLocked(l.state)
}

func (l *localSession) Checkpoint(ctx context.Context) (Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return Checkpoint{}, err
	}
	if !l.current() {
		return Checkpoint{}, ErrSessionClosed
	}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return readSpool(l.state.path, offset, end)
}

func (l *localSession) SpoolPath() string { return l.state.path }

func (l *localSession) SpoolSize(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return spoolSize(l.state.path)
}

func (l *localSession) WatchOutput(options WatchOptions) (*SpoolWatcher, error) {
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
	if !l.current() {
		return nil
	}
	return terminate(l.state)
}

func (l *localSession) Remove() error {
	if !l.current() {
		return nil
	}
	l.hub.remove(l.state.name)
	return terminate(l.state)
}

func (l *localSession) TruncateSpool(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return truncateSpool(l.state.path)
}

func (l *localSession) ArchiveSpool(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return archiveSpool(l.state.path)
}

func (l *localSession) RemoveSpool() { removeSpool(l.state.path) }

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
	defer file.Close()
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
	defer source.Close()
	destination, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := gzip.NewWriter(destination)
	if _, err := io.Copy(writer, source); err != nil {
		_ = writer.Close()
		_ = destination.Close()
		_ = os.Remove(archive)
		return err
	}
	if err := writer.Close(); err != nil {
		_ = destination.Close()
		_ = os.Remove(archive)
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
		oldest := matches[0]
		for _, match := range matches[1:] {
			if match < oldest {
				oldest = match
			}
		}
		_ = os.Remove(oldest)
		matches = removeString(matches, oldest)
	}
	return nil
}

func removeSpool(path string) {
	_ = os.Remove(path)
	for _, match := range mustGlob(path + ".*.gz") {
		_ = os.Remove(match)
	}
}

type sessionStatus struct {
	Alive bool       `json:"alive"`
	Exit  *ExitError `json:"exit,omitempty"`
}

func (s *Session) status() sessionStatus {
	local, ok := s.backend.(*localSession)
	if !ok {
		return sessionStatus{}
	}
	if local.Alive() {
		return sessionStatus{Alive: true}
	}
	local.state.waitMu.Lock()
	defer local.state.waitMu.Unlock()
	exit, _ := convertExit(local.state.waitErr).(*ExitError)
	return sessionStatus{Exit: exit}
}
