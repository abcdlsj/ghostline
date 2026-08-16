package ghostline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Options configures a Hub. Zero values select documented defaults.
type Options struct {
	// OutputDir stores session spools. An empty value uses
	// $HOME/.ghostline/output.
	OutputDir string
	// DefaultSize is used when SessionOptions.Size is zero. The default is
	// 120 columns by 36 rows.
	DefaultSize Size
}

// SessionOptions configures one session.
type SessionOptions struct {
	// Name identifies the session and its spool file. It must be a single,
	// non-empty path component.
	Name string
	// Directory is the child's working directory. An empty value inherits the
	// embedding process's working directory.
	Directory string
	// Command is evaluated by "sh -lc". An empty value starts $SHELL, falling
	// back to sh.
	Command string
	// Size is the initial grid size. A zero value uses the hub's default.
	Size Size
	// Environment entries use KEY=value form and override inherited values.
	Environment []string
}

// Hub owns local pseudo-terminal sessions.
type Hub struct {
	outputDir   string
	defaultSize Size

	mu       sync.Mutex
	sessions map[string]*sessionState
	pending  map[string]struct{}
	closed   bool
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
		outputDir:   dir,
		defaultSize: size,
		sessions:    make(map[string]*sessionState),
		pending:     make(map[string]struct{}),
	}, nil
}

// Start creates and starts a session.
func (h *Hub) Start(ctx context.Context, options SessionOptions) (Session, error) {
	if err := validateName(options.Name); err != nil {
		return nil, err
	}
	if err := validateEnvironment(options.Environment); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size, err := options.Size.resolve(h.defaultSize)
	if err != nil {
		return nil, err
	}

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
	path := filepath.Join(h.outputDir, options.Name+spoolSuffix)
	state, err := startSession(ctx, options, size, path)
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
	return &localSession{hub: h, state: state}, nil
}

// Session returns a handle for a known session name.
func (h *Hub) Session(name string) (Session, bool) {
	state := h.session(name)
	if state == nil {
		return nil, false
	}
	return &localSession{hub: h, state: state}, true
}

// Sessions returns all known sessions ordered by creation time and name.
func (h *Hub) Sessions() []Session {
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
	sessions := make([]Session, 0, len(states))
	for _, state := range states {
		sessions = append(sessions, &localSession{hub: h, state: state})
	}
	return sessions
}

// Check verifies that libghostty-vt can create a terminal.
func (h *Hub) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	terminal, err := NewVTTerminal(1, 1)
	if err != nil {
		return err
	}
	terminal.Close()
	return nil
}

// Close terminates every session and prevents further Start calls.
func (h *Hub) Close() error {
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

func (h *Hub) spoolPath(name string) string {
	if validateName(name) != nil {
		return ""
	}
	return filepath.Join(h.outputDir, name+spoolSuffix)
}
