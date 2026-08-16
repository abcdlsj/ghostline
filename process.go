package ghostline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const (
	spoolSuffix = ".out"
	builtinTerm = "xterm-256color"
)

type sessionState struct {
	name      string
	path      string
	command   *exec.Cmd
	pid       int
	size      Size
	master    *os.File
	spool     *os.File
	vt        *VTTerminal
	createdAt time.Time

	inputMu   sync.Mutex
	outputMu  sync.Mutex
	waitMu    sync.Mutex
	waitErr   error
	watcherMu sync.Mutex
	watchers  []*SpoolWatcher
	closeOnce sync.Once
	done      chan struct{}
	reaped    chan struct{}

	migrationMu       sync.Mutex
	migrationPending  bool
	migrationDone     chan struct{}
	migrationDecision chan bool
	migratedFlag      atomic.Bool
}

func startSession(ctx context.Context, options SessionOptions, size Size, path string, defaultTerm string) (*sessionState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vt, err := NewVTTerminal(size.Columns, size.Rows)
	if err != nil {
		return nil, fmt.Errorf("create vt: %w", err)
	}
	spool, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		vt.Close()
		return nil, fmt.Errorf("open spool: %w", err)
	}
	command := ptyCommand(options.Directory, options.Command, options.Environment, defaultTerm)
	master, err := pty.StartWithSize(command, &pty.Winsize{Cols: uint16(size.Columns), Rows: uint16(size.Rows)})
	if err != nil {
		closeQuietly(spool)
		vt.Close()
		return nil, fmt.Errorf("start pty: %w", err)
	}
	return &sessionState{
		name:      options.Name,
		path:      path,
		command:   command,
		pid:       command.Process.Pid,
		size:      size,
		master:    master,
		spool:     spool,
		vt:        vt,
		createdAt: time.Now(),
		done:      make(chan struct{}),
		reaped:    make(chan struct{}),
	}, nil
}

func copyOutput(state *sessionState) {
	defer close(state.done)
	defer closeQuietly(state.spool)
	defer func() {
		// During migration the master is transferred to the new server and
		// stays open until the embedding process exits.
		if !state.migratedFlag.Load() {
			closeQuietly(state.master)
		}
	}()
	buffer := make([]byte, 32*1024)
	for copyOutputLoop(state, buffer) {
	}
	if state.command != nil {
		waitErr := state.command.Wait()
		state.waitMu.Lock()
		state.waitErr = waitErr
		state.waitMu.Unlock()
	}
	close(state.reaped)
}

// copyOutputLoop drains one session until the child exits or a migration
// handoff is committed. It returns true after an aborted migration so the
// caller keeps serving the session.
func copyOutputLoop(state *sessionState, buffer []byte) bool {
	for {
		if state.migrationRequested() {
			// Drain whatever the child has already written, then stop. Bytes
			// produced afterwards stay buffered in the PTY and are picked up
			// by the new server after it restores the snapshot.
			if err := drainOutput(state, buffer); err != nil {
				return false
			}
			stable := state.migrationStable()
			close(stable)
			if <-state.migrationDecision {
				return false
			}
			return true
		}
		ready, err := waitReadable(state.master, 50*time.Millisecond)
		if err != nil {
			return false
		}
		if !ready {
			continue
		}
		read, readErr := state.master.Read(buffer)
		if read > 0 {
			chunk := buffer[:read]
			state.outputMu.Lock()
			_, _ = state.spool.Write(chunk)
			state.vt.Feed(chunk)
			state.outputMu.Unlock()
		}
		if readErr != nil {
			return false
		}
	}
}

// waitReadable polls file for readable data, returning true when a read will
// not block. PTY masters do not support Go's SetReadDeadline, so the copy
// loop uses poll(2) with a short timeout to notice migration requests.
func waitReadable(file *os.File, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	fds := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLIN}}
	for {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		ready, err := unix.Poll(fds, int(remaining.Milliseconds()))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		if ready == 0 {
			return false, nil
		}
		return fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0, nil
	}
}

// drainOutput consumes everything already readable on the master. It is used
// during migration so the snapshot and spool boundary include all output the
// child produced before the handoff.
func drainOutput(state *sessionState, buffer []byte) error {
	for {
		ready, err := waitReadable(state.master, 0)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
		read, readErr := state.master.Read(buffer)
		if read > 0 {
			chunk := buffer[:read]
			state.outputMu.Lock()
			_, _ = state.spool.Write(chunk)
			state.vt.Feed(chunk)
			state.outputMu.Unlock()
		}
		if readErr != nil {
			return readErr
		}
	}
}

// beginMigration asks copyOutput to stop at the next safe point. The done
// channel is closed once the emulator state and spool are stable.
func (s *sessionState) beginMigration() {
	s.migrationMu.Lock()
	defer s.migrationMu.Unlock()
	if s.migrationPending {
		return
	}
	s.migrationPending = true
	s.migrationDone = make(chan struct{})
	s.migrationDecision = make(chan bool, 1)
	s.migratedFlag.Store(false)
}

// migrationRequested reports whether the session is being handed to another
// server process.
func (s *sessionState) migrationRequested() bool {
	s.migrationMu.Lock()
	defer s.migrationMu.Unlock()
	return s.migrationPending
}

// migrationStable returns the channel closed when copyOutput has drained all
// readable output and the snapshot/spool boundary is stable.
func (s *sessionState) migrationStable() chan struct{} {
	s.migrationMu.Lock()
	defer s.migrationMu.Unlock()
	return s.migrationDone
}

// commitMigration lets copyOutput stop and transfers ownership of the master
// to the adopting process.
func (s *sessionState) commitMigration() {
	s.finishMigration(true)
}

// abortMigration resumes copyOutput and keeps the session on this server.
func (s *sessionState) abortMigration() {
	s.finishMigration(false)
}

func (s *sessionState) finishMigration(commit bool) {
	s.migrationMu.Lock()
	defer s.migrationMu.Unlock()
	if !s.migrationPending {
		return
	}
	s.migratedFlag.Store(commit)
	s.migrationDecision <- commit
	s.migrationPending = false
}

// adoptState builds a session around a PTY master transferred from another
// server process. The child keeps running; only the owner of the master
// changes. The emulator state is restored from the encoded snapshot.
func adoptState(name string, master *os.File, snapshot []byte, size Size, path string, createdAt time.Time, pid int) (*sessionState, error) {
	vt, err := NewVTTerminal(size.Columns, size.Rows)
	if err != nil {
		return nil, fmt.Errorf("create vt: %w", err)
	}
	if err := vt.RestoreState(snapshot); err != nil {
		vt.Close()
		return nil, fmt.Errorf("restore vt state: %w", err)
	}
	spool, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		vt.Close()
		return nil, fmt.Errorf("open spool: %w", err)
	}
	return &sessionState{
		name:      name,
		path:      path,
		pid:       pid,
		size:      size,
		master:    master,
		spool:     spool,
		vt:        vt,
		createdAt: createdAt,
		done:      make(chan struct{}),
		reaped:    make(chan struct{}),
	}, nil
}

func terminate(state *sessionState) error {
	state.closeWatchers()
	select {
	case <-state.done:
		state.close()
		return nil
	default:
	}
	if state.command != nil && state.command.Process != nil {
		_ = syscall.Kill(-state.command.Process.Pid, syscall.SIGHUP)
	} else if state.pid > 0 {
		_ = syscall.Kill(-state.pid, syscall.SIGHUP)
	}
	if !waitFor(state.reaped, terminateGrace) {
		if state.command != nil && state.command.Process != nil {
			_ = syscall.Kill(-state.command.Process.Pid, syscall.SIGKILL)
		} else if state.pid > 0 {
			_ = syscall.Kill(-state.pid, syscall.SIGKILL)
		}
		if !waitFor(state.reaped, terminateWait) {
			state.close()
			return fmt.Errorf("session %s did not reap", state.name)
		}
	}
	select {
	case <-state.done:
	case <-time.After(terminateWait):
	}
	state.close()
	return nil
}

const (
	terminateGrace = time.Second
	terminateWait  = 2 * time.Second
)

func waitFor(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (s *sessionState) close() {
	s.closeOnce.Do(func() {
		closeQuietly(s.master)
		closeQuietly(s.spool)
		s.vt.Close()
	})
}

func (s *sessionState) closeWatchers() {
	s.watcherMu.Lock()
	watchers := s.watchers
	s.watchers = nil
	s.watcherMu.Unlock()
	for _, watcher := range watchers {
		watcher.Close()
	}
}

func ptyCommand(directory, command string, environment []string, defaultTerm string) *exec.Cmd {
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
	cmd.Env = ptyEnvironment(environment, defaultTerm)
	return cmd
}

func ptyEnvironment(overrides []string, defaultTerm string) []string {
	return mergeTermDefault(mergeEnvironment(os.Environ(), overrides), defaultTerm)
}

// mergeTermDefault returns env with TERM set to defaultTerm when env has no
// non-empty TERM. Overrides are merged before this runs, so an explicit
// caller-supplied TERM wins over both inherited values and the default.
func mergeTermDefault(env []string, defaultTerm string) []string {
	if nonEmptyEnv(env, "TERM") {
		return env
	}
	if defaultTerm == "" {
		defaultTerm = builtinTerm
	}
	return mergeEnvironment(env, []string{"TERM=" + defaultTerm})
}

func nonEmptyEnv(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if len(entry) > len(prefix) && strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func mergeEnvironment(base, overrides []string) []string {
	if len(overrides) == 0 {
		return base
	}
	indices := make(map[string]int, len(base))
	for index, entry := range base {
		if separator := strings.IndexByte(entry, '='); separator > 0 {
			indices[entry[:separator]] = index
		}
	}
	for _, entry := range overrides {
		separator := strings.IndexByte(entry, '=')
		key := entry[:separator]
		if index, ok := indices[key]; ok {
			base[index] = entry
			continue
		}
		indices[key] = len(base)
		base = append(base, entry)
	}
	return base
}

func convertExit(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err
	}
	exit := &ExitError{Code: exitErr.ExitCode()}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		exit.Signal = status.Signal().String()
	}
	return exit
}

func ptySetSize(file *os.File, size Size) error {
	return pty.Setsize(file, &pty.Winsize{Cols: uint16(size.Columns), Rows: uint16(size.Rows)})
}
