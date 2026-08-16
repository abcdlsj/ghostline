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
	// Columns is the number of character cells per line.
	Columns int
	// Rows is the number of lines in the grid.
	Rows int
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

// Options configures a Hub. Zero values select documented defaults.
type Options struct {
	// OutputDir stores append-only output spools and session metadata. An empty
	// value uses $HOME/.ghostline/output.
	OutputDir string
	// DefaultSize is used when SessionOptions.Size is zero. The default is
	// 120 columns by 36 rows.
	DefaultSize Size
}

// New constructs a session hub.
func New(options Options) (*Hub, error) {
	size, err := options.DefaultSize.normalized(Size{Columns: defaultColumns, Rows: defaultRows})
	if err != nil {
		return nil, err
	}
	return &Hub{
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
	// Size is the initial terminal grid size. A zero value uses the hub's
	// default size.
	Size Size
	// Environment entries use KEY=value form and override inherited values.
	// TERM and COLORTERM default to xterm-256color and truecolor respectively.
	Environment []string
}

// Start creates and starts a session.
func (p *Hub) Start(ctx context.Context, options SessionOptions) (*Session, error) {
	return p.start(ctx, options)
}

// sessionBackend implements Session operations against a local Hub or a
// remote Server. Session embeds it so both expose the same API.
type sessionBackend interface {
	Name() string
	CreatedAt() time.Time
	Done() <-chan struct{}
	Wait(ctx context.Context) error
	Alive() bool
	Input(ctx context.Context, data []byte) error
	Resize(ctx context.Context, size Size) error
	Snapshot(ctx context.Context) ([]byte, error)
	Checkpoint(ctx context.Context) (Checkpoint, error)
	SpoolPath() string
	SpoolSize(ctx context.Context) (int64, error)
	WatchOutput(options WatchOptions) (*SpoolWatcher, error)
	Close() error
}

// Session is a stable handle to one pseudo-terminal session, local or remote.
type Session struct {
	sessionBackend
}

// Name returns the session's unique name.
func (s *Session) Name() string { return s.sessionBackend.Name() }

// Checkpoint is an atomic screen replay and raw output position. Bytes below
// Offset are represented by Replay; a paused watcher can SkipTo Offset before
// resuming without losing or duplicating output produced around the snapshot.
type Checkpoint struct {
	// Replay is a full VT replay of the visible grid and scrollback.
	Replay []byte
	// Offset is the raw spool byte position covered by Replay.
	Offset int64
}

// Checkpoint captures a replay and its exact spool boundary atomically.
func (s *Session) Checkpoint(ctx context.Context) (Checkpoint, error) {
	return s.sessionBackend.Checkpoint(ctx)
}

// SpoolPath returns the append-only raw output spool path.
func (s *Session) SpoolPath() string { return s.sessionBackend.SpoolPath() }

// SpoolSize returns the current raw output spool size.
func (s *Session) SpoolSize(ctx context.Context) (int64, error) {
	return s.sessionBackend.SpoolSize(ctx)
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
	return s.sessionBackend.WatchOutput(options)
}

// Close terminates the session. It is idempotent for this handle.
func (s *Session) Close() error { return s.sessionBackend.Close() }

// localSession runs a session inside the embedding process.
type localSession struct {
	hub   *Hub
	state *ptySession
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
		return l.state.waitErr
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

func (l *localSession) Input(ctx context.Context, data []byte) error {
	if err := l.requireCurrent(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeSessionInput(l.state, data)
}

func (l *localSession) Resize(ctx context.Context, size Size) error {
	if err := l.requireCurrent(); err != nil {
		return err
	}
	if _, err := size.normalized(Size{}); err != nil {
		return err
	}
	return resizeSession(l.state, size.Columns, size.Rows)
}

func (l *localSession) Snapshot(ctx context.Context) ([]byte, error) {
	if err := l.requireCurrent(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return captureSession(l.state)
}

func (l *localSession) Checkpoint(ctx context.Context) (Checkpoint, error) {
	if err := l.requireCurrent(); err != nil {
		return Checkpoint{}, err
	}
	l.state.outputMu.Lock()
	defer l.state.outputMu.Unlock()
	replay, err := captureSessionLocked(l.state)
	if err != nil {
		return Checkpoint{}, err
	}
	info, err := os.Stat(l.SpoolPath())
	if err != nil {
		return Checkpoint{}, fmt.Errorf("stat output spool: %w", err)
	}
	return Checkpoint{Replay: replay, Offset: info.Size()}, nil
}

func (l *localSession) SpoolPath() string { return l.hub.SpoolPath(l.state.name) }

func (l *localSession) SpoolSize(ctx context.Context) (int64, error) {
	if err := l.requireCurrent(); err != nil {
		return 0, err
	}
	return l.hub.SpoolSize(ctx, l.state.name)
}

func (l *localSession) WatchOutput(options WatchOptions) (*SpoolWatcher, error) {
	if err := l.requireCurrent(); err != nil {
		return nil, err
	}
	watcher, err := NewSpoolWatcher(
		l.SpoolPath(),
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

func (l *localSession) Close() error {
	// The exited record stays in the hub so Sessions and Session(name) can
	// still report it; only Alive goes false. Hub.Kill removes it entirely.
	return l.hub.terminate(l.state)
}

func (l *localSession) current() bool {
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	return l.hub.sessions[l.state.name] == l.state
}

func (l *localSession) requireCurrent() error {
	if !l.current() {
		return ErrSessionClosed
	}
	return nil
}

// Session returns a handle for a managed session name, including sessions
// whose process has already exited but has not been closed or removed.
func (p *Hub) Session(name string) (*Session, bool) {
	state := p.session(name)
	if state == nil {
		return nil, false
	}
	return &Session{&localSession{hub: p, state: state}}, true
}

// Sessions returns all managed sessions ordered by creation time and name,
// including sessions whose process has already exited.
func (p *Hub) Sessions() []*Session {
	p.mu.Lock()
	states := make([]*ptySession, 0, len(p.sessions))
	for _, state := range p.sessions {
		states = append(states, state)
	}
	p.mu.Unlock()
	sort.Slice(states, func(i, j int) bool {
		left, right := states[i], states[j]
		if left.createdAt.Equal(right.createdAt) {
			return left.name < right.name
		}
		return left.createdAt.Before(right.createdAt)
	})
	result := make([]*Session, 0, len(states))
	for _, state := range states {
		result = append(result, &Session{&localSession{hub: p, state: state}})
	}
	return result
}

func (p *Hub) sessionDefaultSize() Size {
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
