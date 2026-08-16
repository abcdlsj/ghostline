package ghostline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const spoolSuffix = ".out"

type sessionState struct {
	name      string
	path      string
	command   *exec.Cmd
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
}

func startSession(ctx context.Context, options SessionOptions, size Size, path string) (*sessionState, error) {
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
	command := ptyCommand(options.Directory, options.Command, options.Environment)
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
	defer closeQuietly(state.master)
	defer closeQuietly(state.spool)
	buffer := make([]byte, 32*1024)
	for {
		read, err := state.master.Read(buffer)
		if read > 0 {
			chunk := buffer[:read]
			state.outputMu.Lock()
			_, _ = state.spool.Write(chunk)
			state.vt.Feed(chunk)
			state.outputMu.Unlock()
		}
		if err != nil {
			break
		}
	}
	waitErr := state.command.Wait()
	state.waitMu.Lock()
	state.waitErr = waitErr
	state.waitMu.Unlock()
	close(state.reaped)
}

func terminate(state *sessionState) error {
	state.closeWatchers()
	select {
	case <-state.done:
		state.close()
		return nil
	default:
	}
	if state.command.Process != nil {
		_ = syscall.Kill(-state.command.Process.Pid, syscall.SIGHUP)
	}
	if !waitFor(state.reaped, terminateGrace) {
		if state.command.Process != nil {
			_ = syscall.Kill(-state.command.Process.Pid, syscall.SIGKILL)
		}
		if !waitFor(state.reaped, terminateWait) {
			state.close()
			return fmt.Errorf("session %s did not reap", state.name)
		}
	}
	<-state.done
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
	cmd.Env = ptyEnvironment(environment)
	return cmd
}

func ptyEnvironment(overrides []string) []string {
	return mergeEnvironment(os.Environ(), overrides)
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
