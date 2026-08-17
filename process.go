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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const (
	spoolSuffix          = ".out"
	builtinTerm          = "xterm-256color"
	migrationDrainWindow = 50 * time.Millisecond
	migrationDrainBudget = 8 << 20
)

type sessionState struct {
	name               string
	path               string
	command            *exec.Cmd
	pid                int
	masterFD           int
	size               Size
	master             *os.File
	spool              *os.File
	vt                 *VTTerminal
	scrollbackMaxBytes uint64
	createdAt          time.Time

	// operationMu is the session's read/write gate. Ordinary operations share
	// it; migration takes the write side and keeps it until commit or abort.
	operationMu sync.RWMutex
	inputMu     sync.Mutex
	outputMu    sync.Mutex
	waitMu      sync.Mutex
	waitErr     error
	watcherMu   sync.Mutex
	watchers    []*SpoolWatcher
	closeOnce   sync.Once
	done        chan struct{}
	reaped      chan struct{}

	migrationMu sync.Mutex
	migration   *migrationTicket
	migrated    atomic.Bool
}

var (
	errMigrationBusy  = errors.New("session migration already in progress")
	errSessionStopped = errors.New("session has already stopped")
	errMigrationStale = errors.New("session migration is no longer active")
)

// migrationTicket is one immutable handoff attempt. Keeping the channels in
// the ticket, instead of on sessionState, prevents a retry from replacing the
// channel that an older copy loop is still waiting on.
type migrationTicket struct {
	alive bool

	stable  chan struct{}
	decide  chan bool
	stopped chan struct{}

	stableOnce  sync.Once
	stoppedOnce sync.Once
	decided     atomic.Bool
	errMu       sync.Mutex
	err         error
}

func newMigrationTicket(alive bool) *migrationTicket {
	return &migrationTicket{
		alive:   alive,
		stable:  make(chan struct{}),
		decide:  make(chan bool, 1),
		stopped: make(chan struct{}),
	}
}

func (t *migrationTicket) markStable(err error) {
	if err != nil {
		t.errMu.Lock()
		if t.err == nil {
			t.err = err
		}
		t.errMu.Unlock()
	}
	t.stableOnce.Do(func() { close(t.stable) })
}

func (t *migrationTicket) error() error {
	t.errMu.Lock()
	defer t.errMu.Unlock()
	return t.err
}

func (t *migrationTicket) choose(commit bool) {
	if t.decided.CompareAndSwap(false, true) {
		t.decide <- commit
	}
}

func (t *migrationTicket) markStopped() {
	t.stoppedOnce.Do(func() { close(t.stopped) })
}

func startSession(ctx context.Context, options SessionOptions, size Size, path string, defaultTerm string, scrollbackMaxBytes uint64) (*sessionState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vt, err := NewVTTerminalWithOptions(size.Columns, size.Rows, VTTerminalOptions{
		ScrollbackMaxBytes: scrollbackMaxBytes,
	})
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
		name:               options.Name,
		path:               path,
		command:            command,
		pid:                command.Process.Pid,
		masterFD:           int(master.Fd()),
		size:               size,
		master:             master,
		spool:              spool,
		vt:                 vt,
		scrollbackMaxBytes: scrollbackMaxBytes,
		createdAt:          time.Now(),
		done:               make(chan struct{}),
		reaped:             make(chan struct{}),
	}, nil
}

func copyOutput(state *sessionState) {
	defer close(state.done)
	defer closeQuietly(state.spool)
	defer func() {
		// A committed migration closes the old duplicate explicitly after the
		// receiver has its fd; aborted and ordinary exits close it here.
		if !state.migrated.Load() && state.master != nil {
			closeQuietly(state.master)
		}
	}()
	// A child can exit and start reaping at the same time a migration ticket
	// is created. If the copy loop has already stopped reading, nothing will
	// ever observe the ticket, and a waiting admin handler would hang. Settle
	// any undecided ticket here, just before done is closed.
	defer func() {
		if ticket := state.currentMigration(); ticket != nil && !ticket.decided.Load() {
			ticket.markStable(errSessionStopped)
			ticket.choose(false)
			ticket.markStopped()
		}
	}()
	buffer := make([]byte, 32*1024)
	for copyOutputLoop(state, buffer) {
	}
	if state.migrated.Load() {
		// Ownership moved with the master fd. Do not keep old waiters blocked
		// on a child whose status belongs to the next server; reap it in the
		// background when this process still owns the child.
		state.waitMu.Lock()
		state.waitErr = &ExitError{Code: -1, Unknown: true}
		state.waitMu.Unlock()
		close(state.reaped)
		if state.command != nil {
			go func(command *exec.Cmd) { _ = command.Wait() }(state.command)
		}
		return
	}
	if state.command != nil {
		waitErr := state.command.Wait()
		state.waitMu.Lock()
		state.waitErr = waitErr
		state.waitMu.Unlock()
	} else if state.master != nil {
		// A migrated child is no longer our OS child, so its original wait
		// status cannot be recovered after this server exits. We still expose a
		// real terminal exit rather than silently turning Wait into success.
		state.waitMu.Lock()
		state.waitErr = &ExitError{Code: -1, Unknown: true}
		state.waitMu.Unlock()
	}
	close(state.reaped)
}

// copyOutputLoop drains one session until the child exits or a migration
// handoff is committed. It returns true after an aborted migration so the
// caller keeps serving the session.
func copyOutputLoop(state *sessionState, buffer []byte) bool {
	for {
		if ticket := state.currentMigration(); ticket != nil && ticket.alive && !ticket.decided.Load() {
			// Drain whatever the child has already written, then stop. Bytes
			// produced afterwards stay buffered in the PTY and are picked up
			// by the new server after it restores the snapshot.
			err := drainOutput(state, buffer)
			ticket.markStable(err)
			commit := <-ticket.decide
			ticket.markStopped()
			if err != nil || commit {
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
	if file == nil {
		return false, os.ErrInvalid
	}
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
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return false, os.ErrInvalid
		}
		return fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0, nil
	}
}

// drainOutput consumes the currently readable master up to a small handoff
// budget. A child that writes forever must not starve the transaction; bytes
// left in the PTY remain queued for the new owner after commit.
func drainOutput(state *sessionState, buffer []byte) error {
	deadline := time.Now().Add(migrationDrainWindow)
	drained := 0
	for {
		if drained >= migrationDrainBudget || time.Now().After(deadline) {
			return nil
		}
		ready, err := waitReadable(state.master, 0)
		if err != nil {
			return err
		}
		if !ready {
			return nil
		}
		read, readErr := state.master.Read(buffer)
		if read > 0 {
			drained += read
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

// beginMigration takes the exclusive session gate and creates one immutable
// handoff ticket. Stopped sessions use the same transaction shape, but have no
// copy loop or PTY fd to pause.
func (s *sessionState) beginMigration() (*migrationTicket, error) {
	s.migrationMu.Lock()
	busy := s.migration != nil
	s.migrationMu.Unlock()
	if busy {
		return nil, errMigrationBusy
	}

	s.operationMu.Lock()

	s.migrationMu.Lock()
	if s.migration != nil {
		s.migrationMu.Unlock()
		s.operationMu.Unlock()
		return nil, errMigrationBusy
	}
	select {
	case <-s.done:
		ticket := newMigrationTicket(false)
		s.migration = ticket
		s.migrationMu.Unlock()
		ticket.markStable(nil)
		ticket.markStopped()
		return ticket, nil
	default:
	}
	ticket := newMigrationTicket(true)
	s.migration = ticket
	s.migrationMu.Unlock()
	return ticket, nil
}

func (s *sessionState) currentMigration() *migrationTicket {
	s.migrationMu.Lock()
	defer s.migrationMu.Unlock()
	return s.migration
}

// finishMigration resolves a ticket, waits for the copy loop to observe the
// decision, and releases the exclusive operation gate.
func (s *sessionState) finishMigration(ticket *migrationTicket, commit bool) error {
	if ticket == nil {
		return errMigrationStale
	}
	if commit && ticket.error() != nil {
		commit = false
	}
	if commit {
		s.migrated.Store(true)
	}
	ticket.choose(commit)
	select {
	case <-ticket.stopped:
	case <-s.done:
		// The copy loop has already exited, so it can never observe the
		// decision. done is closed only after the loop and its finalizers are
		// gone, so there is nothing left to wait for.
	}
	s.migrationMu.Lock()
	if s.migration != ticket {
		s.migrationMu.Unlock()
		s.operationMu.Unlock()
		return errMigrationStale
	}
	s.migration = nil
	s.migrationMu.Unlock()
	if commit && s.master != nil {
		// The receiver owns its SCM_RIGHTS duplicate now. Closing this
		// process's copy keeps an in-process upgrade from retaining a second
		// PTY master forever.
		closeFileQuietly(s.master)
	}
	s.operationMu.Unlock()
	return ticket.error()
}

// adoptState builds a session around a PTY master transferred from another
// server process. The child keeps running; only the owner of the master
// changes. The emulator state is restored from the encoded snapshot.
func adoptState(name string, master *os.File, snapshot []byte, size Size, path string, createdAt time.Time, pid int, exit *ExitError, scrollbackMaxBytes uint64) (*sessionState, error) {
	vt, err := NewVTTerminalWithOptions(size.Columns, size.Rows, VTTerminalOptions{
		ScrollbackMaxBytes: scrollbackMaxBytes,
	})
	if err != nil {
		closeFileQuietly(master)
		return nil, fmt.Errorf("create vt: %w", err)
	}
	if err := vt.RestoreState(snapshot); err != nil {
		vt.Close()
		closeFileQuietly(master)
		return nil, fmt.Errorf("restore vt state: %w", err)
	}
	spool, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		vt.Close()
		closeFileQuietly(master)
		return nil, fmt.Errorf("open spool: %w", err)
	}
	state := &sessionState{
		name:               name,
		path:               path,
		pid:                pid,
		masterFD:           -1,
		size:               size,
		master:             master,
		spool:              spool,
		vt:                 vt,
		scrollbackMaxBytes: scrollbackMaxBytes,
		createdAt:          createdAt,
		done:               make(chan struct{}),
		reaped:             make(chan struct{}),
	}
	if master == nil {
		if exit == nil {
			exit = &ExitError{Code: -1, Unknown: true}
		}
		state.waitErr = exit
		close(state.done)
		close(state.reaped)
	} else {
		state.masterFD = int(master.Fd())
	}
	return state, nil
}

// adoptStateFromSpool rebuilds a session's emulator by replaying the source
// server's archived and live PTY output while the source migration ticket is
// still paused. It is intentionally used only for the bounded compatibility
// window in Adopt; current servers transfer authoritative native snapshots.
func adoptStateFromSpool(ctx context.Context, name string, master *os.File, size Size, path string, createdAt time.Time, pid int, exit *ExitError, scrollbackMaxBytes uint64) (*sessionState, error) {
	vt, err := NewVTTerminalWithOptions(size.Columns, size.Rows, VTTerminalOptions{
		ScrollbackMaxBytes: scrollbackMaxBytes,
	})
	if err != nil {
		closeFileQuietly(master)
		return nil, fmt.Errorf("create vt: %w", err)
	}
	if err := replaySpool(ctx, vt, path); err != nil {
		vt.Close()
		closeFileQuietly(master)
		return nil, err
	}

	spool, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		vt.Close()
		closeFileQuietly(master)
		return nil, fmt.Errorf("open spool: %w", err)
	}
	state := &sessionState{
		name:               name,
		path:               path,
		pid:                pid,
		masterFD:           -1,
		size:               size,
		master:             master,
		spool:              spool,
		vt:                 vt,
		scrollbackMaxBytes: scrollbackMaxBytes,
		createdAt:          createdAt,
		done:               make(chan struct{}),
		reaped:             make(chan struct{}),
	}
	if master == nil {
		if exit == nil {
			exit = &ExitError{Code: -1, Unknown: true}
		}
		state.waitErr = exit
		close(state.done)
		close(state.reaped)
	} else {
		state.masterFD = int(master.Fd())
	}
	return state, nil
}

func replaySpool(ctx context.Context, vt *VTTerminal, path string) error {
	archives, err := filepath.Glob(path + ".*.gz")
	if err != nil {
		return fmt.Errorf("find spool archives: %w", err)
	}
	sort.Strings(archives)
	paths := make([]struct {
		path       string
		compressed bool
	}, 0, len(archives)+1)
	for _, archive := range archives {
		paths = append(paths, struct {
			path       string
			compressed bool
		}{path: archive, compressed: true})
	}
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat spool: %w", err)
		}
		if len(paths) == 0 {
			return fmt.Errorf("stat spool: %w", err)
		}
	} else {
		paths = append(paths, struct {
			path       string
			compressed bool
		}{path: path})
	}

	for _, item := range paths {
		if err := replaySpoolFile(ctx, vt, item.path, item.compressed); err != nil {
			return err
		}
	}
	return nil
}

func replaySpoolFile(ctx context.Context, vt *VTTerminal, path string, compressed bool) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open spool replay %s: %w", path, err)
	}
	defer closeQuietly(file)

	var reader io.Reader = file
	var compressedReader *gzip.Reader
	if compressed {
		compressedReader, err = gzip.NewReader(file)
		if err != nil {
			return fmt.Errorf("open compressed spool replay %s: %w", path, err)
		}
		defer compressedReader.Close()
		reader = compressedReader
	}

	buffer := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			vt.Feed(buffer[:read])
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("replay spool %s: %w", path, readErr)
		}
	}
}

func terminate(state *sessionState) error {
	state.operationMu.Lock()
	defer state.operationMu.Unlock()
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
		if s.master != nil {
			closeQuietly(s.master)
		}
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
	var known *ExitError
	if errors.As(err, &known) {
		return known
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
