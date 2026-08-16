package ghostline

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"
)

const (
	defaultColumns = 120
	defaultRows    = 36
)

// Size is a terminal grid size in cells.
type Size struct {
	Columns int
	Rows    int
}

func (s Size) normalized(fallback Size) (Size, error) {
	if s.Columns == 0 && s.Rows == 0 {
		s = fallback
	}
	if s.Columns <= 0 || s.Rows <= 0 || s.Columns > maxTerminalDimension || s.Rows > maxTerminalDimension {
		return Size{}, fmt.Errorf("invalid terminal size %dx%d", s.Columns, s.Rows)
	}
	return s, nil
}

// Options configures a Manager. Zero values select documented defaults.
type Options struct {
	// OutputDir stores append-only output spools and session metadata. An empty
	// value uses $HOME/.ghostline/output.
	OutputDir string
	// DefaultSize is used when SessionOptions.Size is zero. The default is
	// 120 columns by 36 rows.
	DefaultSize Size
}

// New constructs a session manager.
func New(options Options) (*Manager, error) {
	size, err := options.DefaultSize.normalized(Size{Columns: defaultColumns, Rows: defaultRows})
	if err != nil {
		return nil, err
	}
	return &Manager{
		OutputDir:   options.OutputDir,
		sessions:    make(map[string]*ptySession),
		defaultSize: size,
	}, nil
}

// SessionOptions configures one pseudo-terminal session.
type SessionOptions struct {
	// Name identifies the session and its spool files. It must be a single,
	// non-empty path component.
	Name string
	// Directory is the child's working directory. An empty value inherits the
	// embedding process's working directory.
	Directory string
	// Command is evaluated by "sh -lc". An empty value starts $SHELL, falling
	// back to sh.
	Command string
	// Size is the initial terminal grid size. A zero value uses the manager's
	// default size.
	Size Size
	// Environment entries use KEY=value form and override inherited values.
	// TERM and COLORTERM default to xterm-256color and truecolor respectively.
	Environment []string
}

// Start creates and starts a session.
func (p *Manager) Start(ctx context.Context, options SessionOptions) (*Session, error) {
	return p.start(ctx, options)
}

// Session is a stable handle to one managed pseudo-terminal.
type Session struct {
	manager *Manager
	state   *ptySession
}

// Name returns the session's manager-unique name.
func (s *Session) Name() string {
	if s == nil || s.state == nil {
		return ""
	}
	return s.state.name
}

// CreatedAt returns when the child process was started.
func (s *Session) CreatedAt() time.Time {
	if s == nil || s.state == nil {
		return time.Time{}
	}
	return s.state.createdAt
}

// Done is closed after the child process exits and all of its output has been
// consumed by the manager.
func (s *Session) Done() <-chan struct{} {
	if s == nil || s.state == nil {
		return closedSessionDone
	}
	return s.state.done
}

// Wait waits for the child process and returns its exit error. Context
// cancellation stops waiting but does not terminate the session.
func (s *Session) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.Done():
		if s == nil || s.state == nil {
			return ErrSessionClosed
		}
		s.state.waitMu.Lock()
		defer s.state.waitMu.Unlock()
		return s.state.waitErr
	}
}

// Alive reports whether the handle still refers to a running managed session.
func (s *Session) Alive() bool {
	if !s.current() {
		return false
	}
	select {
	case <-s.state.done:
		return false
	default:
		return true
	}
}

// Input writes bytes to the session's PTY verbatim.
func (s *Session) Input(ctx context.Context, data []byte) error {
	if err := s.requireCurrent(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeSessionInput(s.state, data)
}

// Resize updates the real PTY and the server-side VT emulator.
func (s *Session) Resize(ctx context.Context, size Size) error {
	if err := s.requireCurrent(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := size.normalized(Size{}); err != nil {
		return err
	}
	return resizeSession(s.state, size.Columns, size.Rows)
}

// Snapshot returns a complete VT replay of the visible grid and scrollback.
func (s *Session) Snapshot(ctx context.Context) ([]byte, error) {
	if err := s.requireCurrent(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return captureSession(s.state)
}

// Checkpoint is an atomic screen replay and raw output position. Bytes below
// Offset are represented by Replay; a paused watcher can SkipTo Offset before
// resuming without losing or duplicating output produced around the snapshot.
type Checkpoint struct {
	Replay []byte
	Offset int64
}

// Checkpoint captures a replay and its exact spool boundary atomically.
func (s *Session) Checkpoint(ctx context.Context) (Checkpoint, error) {
	if err := s.requireCurrent(); err != nil {
		return Checkpoint{}, err
	}
	if err := ctx.Err(); err != nil {
		return Checkpoint{}, err
	}
	s.state.outputMu.Lock()
	defer s.state.outputMu.Unlock()
	replay, err := captureSessionLocked(s.state)
	if err != nil {
		return Checkpoint{}, err
	}
	info, err := os.Stat(s.SpoolPath())
	if err != nil {
		return Checkpoint{}, fmt.Errorf("stat output spool: %w", err)
	}
	return Checkpoint{Replay: replay, Offset: info.Size()}, nil
}

// SpoolPath returns the append-only raw output spool path.
func (s *Session) SpoolPath() string {
	if s == nil || s.manager == nil {
		return ""
	}
	return s.manager.SpoolPath(s.Name())
}

// SpoolSize returns the current raw output spool size.
func (s *Session) SpoolSize(ctx context.Context) (int64, error) {
	if err := s.requireCurrent(); err != nil {
		return 0, err
	}
	return s.manager.SpoolSize(ctx, s.state.name)
}

// WatchOptions configures an output subscription.
type WatchOptions struct {
	// Offset is the first spool byte to deliver.
	Offset int64
	// MaxBytes invokes OnOverflow after the watcher passes the limit. Zero uses
	// the watcher default.
	MaxBytes int64
	// OnOutput receives borrowed output bytes. Copy the slice to retain it after
	// the callback returns.
	OnOutput func([]byte)
	// OnTruncate runs when the spool is compacted in place.
	OnTruncate func()
	// OnOverflow runs when Offset passes MaxBytes.
	OnOverflow func()
}

// WatchOutput subscribes to raw output and starts the watcher before returning.
func (s *Session) WatchOutput(options WatchOptions) (*SpoolWatcher, error) {
	if err := s.requireCurrent(); err != nil {
		return nil, err
	}
	watcher, err := NewSpoolWatcher(
		s.SpoolPath(),
		options.Offset,
		options.OnOutput,
		options.OnTruncate,
		options.OnOverflow,
	)
	if err != nil {
		return nil, err
	}
	watcher.SetMaxBytes(options.MaxBytes)
	watcher.Start()
	return watcher, nil
}

// Close terminates the session. It is idempotent for this handle.
func (s *Session) Close() error {
	if s == nil || s.manager == nil || s.state == nil {
		return nil
	}
	s.manager.mu.Lock()
	if s.manager.sessions[s.state.name] != s.state {
		s.manager.mu.Unlock()
		return nil
	}
	delete(s.manager.sessions, s.state.name)
	s.manager.mu.Unlock()
	return s.manager.terminate(s.state)
}

func (s *Session) current() bool {
	if s == nil || s.manager == nil || s.state == nil {
		return false
	}
	s.manager.mu.Lock()
	defer s.manager.mu.Unlock()
	return s.manager.sessions[s.state.name] == s.state
}

func (s *Session) requireCurrent() error {
	if !s.current() {
		return ErrSessionClosed
	}
	return nil
}

// Session returns a handle for a managed session name.
func (p *Manager) Session(name string) (*Session, bool) {
	state := p.session(name)
	if state == nil {
		return nil, false
	}
	return &Session{manager: p, state: state}, true
}

// Sessions returns all managed sessions ordered by creation time and name.
func (p *Manager) Sessions() []*Session {
	p.mu.Lock()
	result := make([]*Session, 0, len(p.sessions))
	for _, state := range p.sessions {
		result = append(result, &Session{manager: p, state: state})
	}
	p.mu.Unlock()
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i].state, result[j].state
		if left.createdAt.Equal(right.createdAt) {
			return left.name < right.name
		}
		return left.createdAt.Before(right.createdAt)
	})
	return result
}

func (p *Manager) sessionDefaultSize() Size {
	if p.defaultSize.Columns > 0 && p.defaultSize.Rows > 0 {
		return p.defaultSize
	}
	return Size{Columns: defaultColumns, Rows: defaultRows}
}

var closedSessionDone = func() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()
