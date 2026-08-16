# RFC 0002: Rolling upgrade for the ghostline server

- Status: Accepted
- Date: 2026-08-16
- Area: Server lifecycle / upgrades

## Summary

Allow a ghostline server process to be upgraded without ending its sessions.
A freshly started server adopts every session from the old server: the PTY
master file descriptor is moved over a Unix socket (`SCM_RIGHTS`), the
server-side libghostty-vt state is serialized with the snapshot API and
restored, and only then does the old server exit. Child processes never see a
disconnect.

## Problem

Today the server owns the PTY masters and the emulated terminal state, so
upgrading the server binary means restarting it, which closes every master
and ends every session. Daemon upgrades are already safe (the daemon reconnects
to the same server), but the server itself cannot change until all sessions
are gone. As ghostline's server code evolves (for example the resize ordering
fix), deployments must choose between stale behavior and losing sessions.

## Goals

- Upgrade the server binary with zero session loss: children keep running and
  the emulated screen state carries over exactly.
- Keep the upgrade window short and invisible to attached clients (output
  pauses for milliseconds at most).
- Fail safely: if adoption fails, the old server keeps serving and nothing is
  lost.

## Non-goals

- Multi-replica or cross-machine session migration (single-host, single
  server).
- Hot-reloading the server in place; the upgrade path is a new process
  adopting from the old one.

## Design

### Trigger and coordination

The embedding daemon (for example Warren) coordinates the upgrade:

1. The daemon starts the new server binary on a fresh socket.
2. The daemon tells the new server where the old server's management socket
   is (socket path + optional token).
3. The new server adopts sessions, then the daemon switches its client to the
   new socket and asks the old server to exit.

The daemon can detect that a rolling upgrade is needed by comparing a server
version advertised on the socket against its own expected version.

### Management socket

The server listens on a second Unix socket next to the main one
(`<socket>.admin`) with a minimal protocol:

- `list` — return session names and metadata (size, spool path, created).
- `adopt <name>` — return the session's `SCM_RIGHTS` master fd, the encoded
  libghostty snapshot, and the current spool offset.
- `exit` — stop accepting, wait for adoptions to finish, then terminate.

Only the new server connects to the management socket; the daemon never talks
to it directly.

### Adoption sequence

For each session, in order:

1. The old server stops draining its master (it does not close it) and stops
   appending to the spool, recording the byte offset. Children may block
   briefly while the PTY buffer fills; the window is milliseconds.
2. The old server encodes the emulator state (`ghostty_snapshot`), then sends
   the master fd and metadata over the management socket.
3. The new server creates the session with the transferred fd and restores
   the snapshot, then starts draining and appending to the same spool from
   the recorded offset. Bytes produced during the window stay buffered in the
   PTY and are drained by the new server, keeping the spool contiguous and
   the emulated state consistent.
4. After all sessions are adopted, the new server signals readiness, the
   daemon switches clients, and the old server exits.

Attached clients see only a sub-second output pause; recovery anchors and
spool offsets remain valid because the spool is never rewound.

### Failure safety

- If adoption fails for any session, the old server aborts the migration and
  keeps serving unchanged.
- The daemon only switches to the new socket after every session is adopted.
- The new server refuses to start serving new sessions until adoption is
  complete, so no split-brain state exists.

## Components

- `VTTerminal.Snapshot` / `Restore` — CGo wrappers around the libghostty
  snapshot API.
- `Server` management socket and adoption protocol.
- A small client-side `Adopt` helper used by a freshly started server.
- Warren daemon coordination: start new server, trigger adoption, switch
  socket, retire old server.

## Implementation notes

- The management protocol is all-or-nothing: the new server prepares every
  session (fd + snapshot) before committing any of them. A failure aborts the
  whole batch and the old server keeps serving.
- PTY masters do not support Go's `SetReadDeadline`, so output draining polls
  with `poll(2)` and stops only after the child's pending bytes are flushed.
- `ghostline serve --adopt-from <admin-socket>` performs the adoption before
  binding its public socket, so clients never observe a half-upgraded server.

## Alternatives considered

- **Restart children and replay the spool.** No process survives; shells and
  agents restart from scratch. Rejected because session state is the point of
  a server-owned runtime.
- **Keep the server on the old binary forever.** Delays fixes (the resize
  ordering bug is a recent example). Rejected as a dead end.

## Open questions

- Does `ghostty_snapshot` cover every state the emulator tracks (terminal
  modes, kitty graphics, OSC state)? A spike with Codex should verify exact
  screen equality before and after adoption.
- How long can a child block while the PTY buffer fills during the window?
  High-throughput output could need a larger migration buffer.
- Version compatibility: only "new server adopts from old server" is
  supported; the protocol should document a minimum old-server version.
