package ghostline

import "errors"

var (
	// ErrUnavailable indicates that libghostty-vt cannot be used by this build.
	ErrUnavailable = errors.New("ghostline is unavailable")
	// ErrClosed indicates that an operation requires an open hub.
	ErrClosed = errors.New("ghostline hub is closed")
	// ErrSessionExists indicates that a hub already owns the requested name.
	ErrSessionExists = errors.New("ghostline session already exists")
	// ErrSessionNotFound indicates that a hub does not own the requested name.
	ErrSessionNotFound = errors.New("ghostline session not found")
	// ErrSessionClosed indicates that a Session handle no longer refers to a
	// session owned by its hub.
	ErrSessionClosed = errors.New("ghostline session is closed")
	// ErrInvalidSessionName indicates that a name is empty or unsafe for use as
	// a spool filename.
	ErrInvalidSessionName = errors.New("invalid ghostline session name")
)
