// Package ghostline provides a local-first terminal session runtime for Go.
//
// A Hub owns pseudo-terminals and child processes in the embedding process. A
// Server and Client provide the same concrete Session contract over a
// same-host Unix socket. Session identity accessors are cached; every operation
// that can perform process, storage, or network I/O accepts a context and
// returns an error.
//
// Raw PTY output is stored in immutable generations plus one active segment.
// Output returns a bounded reader positioned by an opaque Cursor. Checkpoint
// atomically pairs a terminal replay with the cursor of the first raw byte not
// represented by that replay. Callers own reader cancellation, goroutines,
// archive format, and retention policy.
//
// Sessions survive client detach and successful same-version daemon adoption.
// They do not survive daemon crashes that lose PTY ownership, host reboot, or
// cross-machine migration.
package ghostline
