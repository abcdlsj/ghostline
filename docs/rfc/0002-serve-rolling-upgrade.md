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

- `list` — return session names and metadata (size, created, pid).
- `adopt <name>` — pause the session and return its metadata plus the
  `SCM_RIGHTS` master fd.
- `snapshot <name>` — return the encoded libghostty snapshot for a prepared
  session.
- `commit <names>` / `abort <names>` — commit or abort a prepared batch.
- `exit` — stop accepting, wait for pending adoptions to finish, then
  terminate.

Only the new server connects to the management socket; the daemon never talks
to it directly.

### Adoption sequence

The new server prepares every session before committing any of them:

1. For each session, the new server sends `adopt`. The old server drains
   output that is already readable, so the spool and emulator state include
   everything the child produced so far, then stops reading. Bytes produced
   during the pause stay buffered in the PTY. The master is not closed.
2. The old server sends the master fd and metadata. The new server requests
   `snapshot`, restores the emulator state, and opens the same spool with
   `O_APPEND`. No spool offset is transferred: the new server simply continues
   appending at the current end.
3. When every session is prepared, the new server sends `commit`. The old
   server releases each paused session and the new server starts draining the
   transferred masters. Bytes buffered during the pause are read by the new
   server, keeping the spool contiguous and the emulated state consistent.
4. The new server binds its public socket only after adoption returns, the
   daemon switches clients to it, and the old server exits via `exit`.

Attached clients see only a sub-second output pause; recovery anchors and
spool offsets remain valid because the spool is never rewound or truncated.

### Failure safety

- If adoption fails for any session, the old server aborts the migration and
  keeps serving unchanged.
- The daemon only switches to the new socket after every session is adopted.
- The new server refuses to start serving new sessions until adoption is
  complete, so no split-brain state exists.
- If adoption is impossible (for example the old server predates the admin
  socket), an embedding daemon should keep the old server running and retry
  on a later start rather than ending sessions.

## Components

- `VTTerminal.EncodeState` / `RestoreState` — CGo wrappers around the
  libghostty snapshot API.
- `Server` management socket and adoption protocol.
- `Adopt` and `Server.Adopt` helpers used by a freshly started server.
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
- No spool offset is transferred; the new server opens the existing spool with
  `O_APPEND`, so all previously published spool offsets stay valid.

## Alternatives considered

- **Restart children and replay the spool.** No process survives; shells and
  agents restart from scratch. Rejected because session state is the point of
  a server-owned runtime.
- **Keep the server on the old binary forever.** Delays fixes (the resize
  ordering bug is a recent example). Rejected as a dead end.

## Compatibility and known limitations

- Both servers must speak the admin-socket protocol (ghostline v0.3.4 or
  newer). An older old server cannot be adopted; the daemon keeps serving from
  it until it is restarted for another reason.
- The snapshot covers the emulator's core state (grid, scrollback, cursor,
  terminal modes). Exotic features such as kitty graphics or OSC state are
  expected to travel with the snapshot but are not separately verified.
- The pause window is bounded by the drain step plus snapshot transfer. A
  high-throughput child can block briefly while the PTY buffer fills; the
  design does not add a separate migration buffer.
