package ghostline

import (
	"errors"
	"strconv"
)

var (
	// ErrUnavailable indicates that libghostty-vt cannot be used by this build.
	ErrUnavailable = errors.New("ghostline: libghostty-vt unavailable")
	// ErrClosed indicates that the hub is closed.
	ErrClosed = errors.New("ghostline: hub closed")
	// ErrSessionExists indicates that the name is already taken.
	ErrSessionExists = errors.New("ghostline: session already exists")
	// ErrSessionNotFound indicates that no session has the requested name.
	ErrSessionNotFound = errors.New("ghostline: session not found")
	// ErrSessionClosed indicates that a session handle is no longer usable.
	ErrSessionClosed = errors.New("ghostline: session closed")
	// ErrInvalidSessionName indicates that a name cannot identify a spool file.
	ErrInvalidSessionName = errors.New("ghostline: invalid session name")
)

// ExitError describes a terminated child process.
type ExitError struct {
	// Code is the process exit status, or -1 when the process was signaled.
	Code int
	// Signal names the terminating signal when the process was signaled.
	Signal string
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Signal != "" {
		return "signal: " + e.Signal
	}
	return "exit status " + strconv.Itoa(e.Code)
}
