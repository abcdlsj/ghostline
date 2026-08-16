package ghostline

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Hub owns a set of pseudo-terminal sessions. Sessions keep running while
// clients disconnect, but they remain children of the embedding process and
// therefore do not survive that process exiting.
type Hub struct {
	// OutputDir is the directory used for spool and metadata files.
	//
	// Deprecated: configure Options.OutputDir when constructing the hub.
	OutputDir string

	mu          sync.Mutex
	sessions    map[string]*ptySession
	closed      bool
	defaultSize Size
}

// PTY is the compatibility name for Hub.
// Deprecated: use Hub and New.
type PTY = Hub

type ptySession struct {
	name      string
	command   *exec.Cmd
	master    *os.File
	spool     *os.File
	vt        *VTTerminal
	createdAt time.Time

	inputMu   sync.Mutex
	outputMu  sync.Mutex
	waitMu    sync.Mutex
	waitErr   error
	closeOnce sync.Once
	done      chan struct{}
	reaped    chan struct{}
}

// NewPTY constructs a hub with the legacy constructor.
// Deprecated: use New with Options.
func NewPTY(outputDir string) *PTY {
	hub, _ := New(Options{OutputDir: outputDir})
	return hub
}

// Check reports whether the hub can construct a libghostty-vt terminal.
func (p *Hub) Check(ctx context.Context) error {
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

// Create starts a session through the legacy name-based API.
func (p *Hub) Create(ctx context.Context, runtimeName, directory, command string) error {
	_, err := p.Start(ctx, SessionOptions{
		Name:      runtimeName,
		Directory: directory,
		Command:   command,
	})
	return err
}

func (p *Hub) start(ctx context.Context, options SessionOptions) (*Session, error) {
	runtimeName := options.Name
	if err := validateRuntimeName(runtimeName); err != nil {
		return nil, err
	}
	if err := validateEnvironment(options.Environment); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size, err := options.Size.normalized(p.sessionDefaultSize())
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	if p.sessions[runtimeName] != nil {
		// A live session keeps its name. An exited one may be replaced by a
		// fresh session with the same name; the old handle then fails with
		// ErrSessionClosed because the map now points at the replacement.
		select {
		case <-p.sessions[runtimeName].done:
			_ = os.Truncate(p.SpoolPath(runtimeName), 0)
			delete(p.sessions, runtimeName)
		default:
			return nil, fmt.Errorf("%w: %s", ErrSessionExists, runtimeName)
		}
	}

	if err := os.MkdirAll(p.outputDir(), 0o700); err != nil {
		return nil, fmt.Errorf("create runtime output directory: %w", err)
	}
	vt, err := NewVTTerminal(size.Columns, size.Rows)
	if err != nil {
		return nil, fmt.Errorf("create ghostty vt: %w", err)
	}
	spool, err := os.OpenFile(p.SpoolPath(runtimeName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		vt.Close()
		return nil, fmt.Errorf("open output spool: %w", err)
	}
	cmd := ptyCommand(options.Directory, options.Command, options.Environment)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(size.Columns), Rows: uint16(size.Rows)})
	if err != nil {
		_ = spool.Close()
		vt.Close()
		return nil, fmt.Errorf("start pty: %w", err)
	}
	session := &ptySession{
		name:      runtimeName,
		command:   cmd,
		master:    master,
		spool:     spool,
		vt:        vt,
		createdAt: time.Now(),
		done:      make(chan struct{}),
		reaped:    make(chan struct{}),
	}
	// Persist creation metadata so a restarted daemon can still identify and
	// reclaim orphaned children (see ListCreated and Kill).
	if err := os.WriteFile(p.CreatedPath(runtimeName), []byte(strconv.FormatInt(session.createdAt.Unix(), 10)+"\n"), 0o600); err != nil {
		abortStartedSession(session)
		return nil, fmt.Errorf("write session creation metadata: %w", err)
	}
	if err := os.WriteFile(p.PIDPath(runtimeName), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		_ = os.Remove(p.CreatedPath(runtimeName))
		abortStartedSession(session)
		return nil, fmt.Errorf("write session process metadata: %w", err)
	}
	p.sessions[runtimeName] = session
	go p.copyOutput(session)
	return &Session{&localSession{hub: p, state: session}}, nil
}

// Close terminates every managed session and releases its resources. A closed
// runtime cannot create new sessions.
func (p *Hub) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	sessions := make([]*ptySession, 0, len(p.sessions))
	for name, session := range p.sessions {
		sessions = append(sessions, session)
		delete(p.sessions, name)
	}
	p.mu.Unlock()

	var errs []error
	for _, session := range sessions {
		if err := p.terminate(session); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func abortStartedSession(session *ptySession) {
	if session.command.Process != nil {
		_ = syscall.Kill(-session.command.Process.Pid, syscall.SIGKILL)
		_ = session.command.Process.Kill()
	}
	_ = session.master.Close()
	_ = session.command.Wait()
	session.close()
}

// ptyCommand builds the child command. An empty command starts the user's
// shell; otherwise the command is executed through sh -lc.
func ptyCommand(directory, command string, environment []string) *exec.Cmd {
	var cmd *exec.Cmd
	if strings.TrimSpace(command) == "" {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "sh"
		}
		cmd = exec.Command(shell)
	} else {
		cmd = exec.Command("sh", "-lc", command)
	}
	cmd.Dir = directory
	// A service process may otherwise inherit an empty TERM and cause programs
	// to suppress colors. Pin a stable color-capable default for every child.
	cmd.Env = ptyEnvironment(environment)
	return cmd
}

func ptyEnvironment(overrides []string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "TERM=") ||
			strings.HasPrefix(item, "COLORTERM=") ||
			// The embedding process may export NO_COLOR (for example when launched
			// from an agent session). It must not leak into PTY children:
			// ghostline pins a color-capable terminal and the client renderer
			// handles colors itself, so applications like Codex would
			// otherwise silently downgrade to monochrome.
			strings.HasPrefix(item, "NO_COLOR=") {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
	return mergeEnvironment(env, overrides)
}

// copyOutput drains the PTY master into the append-only spool, then reaps
// the child. The spool file descriptor stays O_APPEND, so TruncateSpool can
// compact the file in place without stopping the drain.
func (p *Hub) copyOutput(session *ptySession) {
	defer close(session.done)
	defer session.master.Close()
	defer session.spool.Close()
	buffer := make([]byte, 32*1024)
	for {
		read, err := session.master.Read(buffer)
		if read > 0 {
			chunk := buffer[:read]
			session.outputMu.Lock()
			_, _ = session.spool.Write(chunk)
			session.vt.Feed(chunk)
			session.outputMu.Unlock()
		}
		if err != nil {
			break
		}
	}
	waitErr := session.command.Wait()
	session.waitMu.Lock()
	session.waitErr = waitErr
	session.waitMu.Unlock()
	close(session.reaped)
}

// EnsurePipe verifies that a session exists. It is retained for compatibility
// with adapters that install output pipes lazily; Hub owns its spool from
// session creation and therefore needs no additional setup.
func (p *Hub) EnsurePipe(_ context.Context, runtimeName string) error {
	if err := validateRuntimeName(runtimeName); err != nil {
		return err
	}
	if p.session(runtimeName) == nil {
		return fmt.Errorf("pty session not found: %s", runtimeName)
	}
	return nil
}

// Input writes bytes to a named session's PTY verbatim.
func (p *Hub) Input(_ context.Context, runtimeName string, data []byte) error {
	if err := validateRuntimeName(runtimeName); err != nil {
		return err
	}
	session := p.session(runtimeName)
	if session == nil {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, runtimeName)
	}
	return writeSessionInput(session, data)
}

func writeSessionInput(session *ptySession, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	session.inputMu.Lock()
	defer session.inputMu.Unlock()
	if _, err := session.master.Write(data); err != nil {
		return fmt.Errorf("write pty input: %w", err)
	}
	return nil
}

// Resize updates a named session's PTY and VT grid.
func (p *Hub) Resize(_ context.Context, runtimeName string, columns, rows int) error {
	if columns <= 0 || rows <= 0 {
		return nil
	}
	if columns > maxTerminalDimension || rows > maxTerminalDimension {
		return fmt.Errorf("invalid terminal size %dx%d", columns, rows)
	}
	if err := validateRuntimeName(runtimeName); err != nil {
		return err
	}
	session := p.session(runtimeName)
	if session == nil {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, runtimeName)
	}
	return resizeSession(session, columns, rows)
}

func resizeSession(session *ptySession, columns, rows int) error {
	session.inputMu.Lock()
	defer session.inputMu.Unlock()
	if err := pty.Setsize(session.master, &pty.Winsize{Cols: uint16(columns), Rows: uint16(rows)}); err != nil {
		return err
	}
	session.vt.Resize(columns, rows)
	return nil
}

// Capture renders the current emulated screen (visible grid + scrollback)
// with SGR styles preserved, so the client can replay a complete snapshot at
// its own size. This replaces the raw spool replay, which could not restore
// the screen when the PTY history was produced at a different size.
func (p *Hub) Capture(_ context.Context, runtimeName string) ([]byte, error) {
	if err := validateRuntimeName(runtimeName); err != nil {
		return nil, err
	}
	session := p.session(runtimeName)
	if session == nil {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, runtimeName)
	}
	return captureSession(session)
}

func captureSession(session *ptySession) ([]byte, error) {
	session.outputMu.Lock()
	defer session.outputMu.Unlock()
	return captureSessionLocked(session)
}

func captureSessionLocked(session *ptySession) ([]byte, error) {
	snapshot, err := session.vt.Snapshot()
	if err != nil {
		return nil, err
	}
	result := make([]byte, 0, len(snapshot)+7)
	result = append(result, "\x1b[3J\x1b[2J\x1b[H"...)
	result = append(result, snapshot...)
	return result, nil
}

// Recover returns the spool bytes in [offset, end), the raw PTY output a
// client still needs after its anchor. Callers can prefer this over a full
// snapshot whenever the spool still covers the anchor, so switching back to a
// retained surface renders the missing tail without clearing the screen.
func (p *Hub) Recover(_ context.Context, runtimeName string, offset, end int64) ([]byte, error) {
	if err := validateRuntimeName(runtimeName); err != nil {
		return nil, err
	}
	if p.session(runtimeName) == nil {
		return nil, fmt.Errorf("pty session not found: %s", runtimeName)
	}
	file, err := os.Open(p.SpoolPath(runtimeName))
	if err != nil {
		return nil, fmt.Errorf("open output spool: %w", err)
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

// Kill terminates a named session. If the current hub does not own it,
// Kill uses persisted PID metadata to reclaim a process from an earlier run.
func (p *Hub) Kill(_ context.Context, runtimeName string) error {
	if err := validateRuntimeName(runtimeName); err != nil {
		return err
	}
	session := p.session(runtimeName)
	if session != nil {
		p.removeSession(runtimeName)
		return p.terminate(session)
	}
	// Unknown to this process: the daemon restarted and the metadata files
	// are the only handle to the orphaned child.
	pid := readPID(p.PIDPath(runtimeName))
	if pid <= 0 {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	time.Sleep(500 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return nil
}

func (p *Hub) terminate(session *ptySession) error {
	select {
	case <-session.done:
		// The child already exited and was reaped by copyOutput.
		session.close()
		return nil
	default:
	}
	if session.command.Process != nil {
		_ = syscall.Kill(-session.command.Process.Pid, syscall.SIGHUP)
	}
	select {
	case <-session.reaped:
	case <-time.After(time.Second):
		if session.command.Process != nil {
			_ = syscall.Kill(-session.command.Process.Pid, syscall.SIGKILL)
		}
		<-session.reaped
	}
	session.close()
	return nil
}

func (s *ptySession) close() {
	s.closeOnce.Do(func() {
		_ = s.master.Close()
		_ = s.spool.Close()
		s.vt.Close()
	})
}

// Exists reports whether a named session is currently running.
func (p *Hub) Exists(_ context.Context, runtimeName string) bool {
	if validateRuntimeName(runtimeName) != nil {
		return false
	}
	session := p.session(runtimeName)
	if session == nil {
		return false
	}
	select {
	case <-session.done:
		return false
	default:
		return true
	}
}

// List returns the names of all currently running sessions.
func (p *Hub) List(context.Context) (map[string]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sessions := make(map[string]bool, len(p.sessions))
	for name, session := range p.sessions {
		select {
		case <-session.done:
		default:
			sessions[name] = true
		}
	}
	return sessions, nil
}

// ListCreated returns persisted session creation times. A restarted embedding
// process can use them to identify and reclaim children it no longer owns.
func (p *Hub) ListCreated(context.Context) (map[string]time.Time, error) {
	matches, err := filepath.Glob(filepath.Join(p.outputDir(), "*"+spoolSuffix+createdSuffix))
	if err != nil {
		return nil, err
	}
	sessions := make(map[string]time.Time, len(matches))
	for _, match := range matches {
		name := strings.TrimSuffix(filepath.Base(match), spoolSuffix+createdSuffix)
		data, err := os.ReadFile(match)
		if err != nil {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			continue
		}
		sessions[name] = time.Unix(seconds, 0)
	}
	return sessions, nil
}

// SpoolPath returns the raw output spool path, or an empty string for an
// invalid session name.
func (p *Hub) SpoolPath(runtimeName string) string {
	if validateRuntimeName(runtimeName) != nil {
		return ""
	}
	return filepath.Join(p.outputDir(), runtimeName+spoolSuffix)
}

// CreatedPath returns the persisted creation metadata path.
func (p *Hub) CreatedPath(runtimeName string) string {
	path := p.SpoolPath(runtimeName)
	if path == "" {
		return ""
	}
	return path + createdSuffix
}

// PIDPath returns the persisted child process ID metadata path.
func (p *Hub) PIDPath(runtimeName string) string {
	path := p.SpoolPath(runtimeName)
	if path == "" {
		return ""
	}
	return path + pidSuffix
}

// SpoolSize returns the number of raw output bytes currently persisted.
func (p *Hub) SpoolSize(_ context.Context, runtimeName string) (int64, error) {
	if err := validateRuntimeName(runtimeName); err != nil {
		return 0, err
	}
	info, err := os.Stat(p.SpoolPath(runtimeName))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// TruncateSpool compacts the live spool in place. The copyOutput goroutine
// keeps its O_APPEND file descriptor, so output continues into the same
// inode from byte zero. Consumers should reset their offsets and reanchor.
func (p *Hub) TruncateSpool(_ context.Context, runtimeName string) error {
	if err := validateRuntimeName(runtimeName); err != nil {
		return err
	}
	if err := os.Truncate(p.SpoolPath(runtimeName), 0); err != nil {
		return fmt.Errorf("truncate output spool: %w", err)
	}
	return nil
}

// ArchiveSpool compresses the current spool to a timestamped .gz file and
// prunes old archives. Best-effort diagnostics; truncation must not depend
// on archive success.
func (p *Hub) ArchiveSpool(_ context.Context, runtimeName string) error {
	if err := validateRuntimeName(runtimeName); err != nil {
		return err
	}
	path := p.SpoolPath(runtimeName)
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
	return p.pruneArchives(path)
}

func (p *Hub) pruneArchives(path string) error {
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

// RemoveSpool removes a session's spool, metadata, and archives. Callers must
// terminate the session and close its watchers first.
func (p *Hub) RemoveSpool(runtimeName string) {
	if validateRuntimeName(runtimeName) != nil {
		return
	}
	_ = os.Remove(p.SpoolPath(runtimeName))
	_ = os.Remove(p.CreatedPath(runtimeName))
	_ = os.Remove(p.PIDPath(runtimeName))
	for _, match := range mustGlob(p.SpoolPath(runtimeName) + ".*.gz") {
		_ = os.Remove(match)
	}
}

func (p *Hub) outputDir() string {
	if p.OutputDir != "" {
		return p.OutputDir
	}
	return defaultOutputDirectory()
}

func (p *Hub) session(name string) *ptySession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[name]
}

func (p *Hub) removeSession(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, name)
}

func readPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

func validateRuntimeName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") || strings.IndexByte(name, 0) >= 0 {
		return fmt.Errorf("%w: %q", ErrInvalidSessionName, name)
	}
	return nil
}

const (
	spoolSuffix          = ".out"
	createdSuffix        = ".created"
	pidSuffix            = ".pid"
	maxTerminalDimension = 1<<16 - 1
)
