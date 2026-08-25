package ghostline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Options configures a Hub. Zero values select documented defaults.
type Options struct {
	// OutputDir stores segmented session output. An empty value uses
	// $HOME/.ghostline/output.
	OutputDir string
	// DefaultSize is used when SessionOptions.Size is zero. The default is
	// 120 columns by 36 rows.
	DefaultSize Size
	// DefaultTerm is used for pty children whose environment has no
	// non-empty TERM. An empty value defaults to xterm-256color.
	DefaultTerm string
	// VTScrollbackMaxBytes is the default logical scrollback budget for new
	// sessions. Zero uses DefaultVTScrollbackMaxBytes.
	VTScrollbackMaxBytes uint64
	// ProbeForeground enables OS-level foreground process/cwd metadata.
	// Disabled by default; Session.Metadata returns empty values without
	// spawning any OS probes.
	ProbeForeground bool
	// ServerMaxClientConnections limits concurrently active client socket
	// connections when these options are passed to NewServer. A connection is
	// active for the life of an Output, Replay, or Checkpoint stream. Zero uses
	// DefaultServerMaxClientConnections. Hub ignores this field.
	ServerMaxClientConnections int
}

// ProcessSpec describes the process started inside a session. Path and Args
// are the primary, shell-free form. ShellCommand is explicit opt-in shell
// evaluation and cannot be combined with Path or Args. A zero ProcessSpec
// starts $SHELL, falling back to sh.
type ProcessSpec struct {
	// Path is the executable path. Empty starts the user's shell when
	// ShellCommand and Args are also empty.
	Path string
	// Args are passed directly to Path without shell evaluation.
	Args []string
	// Directory is the child process working directory. Empty inherits the
	// parent process working directory.
	Directory string
	// Environment overrides inherited variables using KEY=VALUE entries.
	Environment []string
	// ShellCommand is evaluated by "sh -lc" and cannot be combined with Path
	// or Args.
	ShellCommand string
}

// Shell returns a process specification evaluated by "sh -lc".
func Shell(command string) ProcessSpec {
	return ProcessSpec{ShellCommand: command}
}

func (p ProcessSpec) validate() error {
	if p.ShellCommand != "" && (p.Path != "" || len(p.Args) != 0) {
		return errors.New("ghostline: shell command cannot be combined with path or args")
	}
	if p.Path == "" && len(p.Args) != 0 {
		return errors.New("ghostline: process args require a path")
	}
	return validateEnvironment(p.Environment)
}

// SessionOptions configures one session.
type SessionOptions struct {
	// Name identifies the session and its output storage. It must be a single,
	// non-empty path component.
	Name string
	// Process describes the child process. Its zero value starts the user's
	// shell without evaluating a command string.
	Process ProcessSpec
	// Size is the initial grid size. A zero value uses the hub's default.
	Size Size
	// VTScrollbackMaxBytes overrides the Hub default for this session. Zero
	// inherits the Hub setting.
	VTScrollbackMaxBytes uint64
}

// Hub owns local pseudo-terminal sessions.
type Hub struct {
	outputDir                   string
	defaultSize                 Size
	defaultTerm                 string
	defaultVTScrollbackMaxBytes uint64
	probeForeground             bool

	// lifecycleMu is a reader/writer gate around hub-wide mutations. Normal
	// session operations take a read lock; a rolling-upgrade batch takes the
	// write lock so no session can appear or disappear halfway through the
	// handoff.
	lifecycleMu sync.RWMutex
	mu          sync.Mutex
	sessions    map[string]*sessionState
	pending     map[string]struct{}
	closed      bool
}

// New constructs a session hub.
func New(options Options) (*Hub, error) {
	size, err := options.DefaultSize.resolve(Size{Columns: defaultColumns, Rows: defaultRows})
	if err != nil {
		return nil, err
	}
	dir := options.OutputDir
	if dir == "" {
		dir = defaultOutputDirectory()
	}
	return &Hub{
		outputDir:                   dir,
		defaultSize:                 size,
		defaultTerm:                 options.DefaultTerm,
		defaultVTScrollbackMaxBytes: resolveVTScrollbackMaxBytes(options.VTScrollbackMaxBytes, DefaultVTScrollbackMaxBytes),
		probeForeground:             options.ProbeForeground,
		sessions:                    make(map[string]*sessionState),
		pending:                     make(map[string]struct{}),
	}, nil
}

// Start creates and starts a session.
func (h *Hub) Start(ctx context.Context, options SessionOptions) (*Session, error) {
	h.lifecycleMu.RLock()
	defer h.lifecycleMu.RUnlock()

	if err := validateName(options.Name); err != nil {
		return nil, err
	}
	if err := options.Process.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size, err := options.Size.resolve(h.defaultSize)
	if err != nil {
		return nil, err
	}
	scrollbackMaxBytes := resolveVTScrollbackMaxBytes(options.VTScrollbackMaxBytes, h.defaultVTScrollbackMaxBytes)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	if _, exists := h.pending[options.Name]; exists || h.sessions[options.Name] != nil {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, options.Name)
	}
	h.pending[options.Name] = struct{}{}
	h.mu.Unlock()

	release := func() {
		h.mu.Lock()
		delete(h.pending, options.Name)
		h.mu.Unlock()
	}
	if err := os.MkdirAll(h.outputDir, 0o700); err != nil {
		release()
		return nil, fmt.Errorf("create output dir: %w", err)
	}
	state, err := startSession(ctx, options, size, h.outputDir, h.defaultTerm, scrollbackMaxBytes)
	if err != nil {
		release()
		return nil, err
	}

	h.mu.Lock()
	delete(h.pending, options.Name)
	if h.closed {
		h.mu.Unlock()
		_ = terminate(state)
		return nil, ErrClosed
	}
	if h.sessions[options.Name] != nil {
		h.mu.Unlock()
		_ = terminate(state)
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, options.Name)
	}
	h.sessions[options.Name] = state
	h.mu.Unlock()
	go copyOutput(state)
	backend := &localSession{hub: h, state: state}
	return newSession(backend, SessionInfo{Name: state.name, CreatedAt: state.createdAt}), nil
}

func resolveVTScrollbackMaxBytes(value, fallback uint64) uint64 {
	if value != 0 {
		return value
	}
	return fallback
}

// Get returns a handle for a known session name.
func (h *Hub) Get(ctx context.Context, name string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.lifecycleMu.RLock()
	defer h.lifecycleMu.RUnlock()
	state := h.session(name)
	if state == nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, name)
	}
	backend := &localSession{hub: h, state: state}
	return newSession(backend, SessionInfo{Name: state.name, CreatedAt: state.createdAt}), nil
}

// List returns all known sessions ordered by creation time and name.
func (h *Hub) List(ctx context.Context) ([]*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.lifecycleMu.RLock()
	defer h.lifecycleMu.RUnlock()
	states := h.sessionStates()
	sessions := make([]*Session, 0, len(states))
	for _, state := range states {
		backend := &localSession{hub: h, state: state}
		sessions = append(sessions, newSession(backend, SessionInfo{
			Name: state.name, CreatedAt: state.createdAt,
		}))
	}
	return sessions, nil
}

// sessionStates returns all internal session states ordered by creation time
// and name. It is used by the admin protocol, which needs fields that are not
// part of the public Session interface.
func (h *Hub) sessionStates() []*sessionState {
	h.mu.Lock()
	states := make([]*sessionState, 0, len(h.sessions))
	for _, state := range h.sessions {
		states = append(states, state)
	}
	h.mu.Unlock()
	sort.Slice(states, func(i, j int) bool {
		left, right := states[i], states[j]
		if left.createdAt.Equal(right.createdAt) {
			return left.name < right.name
		}
		return left.createdAt.Before(right.createdAt)
	})
	return states
}

// Check verifies that libghostty-vt can create a terminal.
func (h *Hub) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	terminal, err := newVTTerminal(1, 1)
	if err != nil {
		return err
	}
	terminal.Close()
	return nil
}

// Close terminates every session and prevents further Start calls.
func (h *Hub) Close() error {
	h.lifecycleMu.Lock()
	defer h.lifecycleMu.Unlock()

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	states := make([]*sessionState, 0, len(h.sessions))
	for name, state := range h.sessions {
		states = append(states, state)
		delete(h.sessions, name)
	}
	h.mu.Unlock()

	var errs []error
	for _, state := range states {
		if err := terminate(state); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (h *Hub) session(name string) *sessionState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[name]
}

func (h *Hub) remove(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, name)
}

// beginMigrationBatch freezes hub-wide mutations until endMigrationBatch.
// The admin protocol holds this write lock from list through commit/abort, so
// its session inventory remains closed under the entire transaction.
func (h *Hub) beginMigrationBatch() bool {
	h.lifecycleMu.Lock()
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		h.lifecycleMu.Unlock()
		return false
	}
	return true
}

func (h *Hub) endMigrationBatch() {
	h.lifecycleMu.Unlock()
}
