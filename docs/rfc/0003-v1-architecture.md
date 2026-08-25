# RFC 0003: v1 architecture and ownership

- Status: Accepted
- Date: 2026-08-24
- Area: Public contract

## Position

ghostline v1 is a local-first terminal session runtime. The primary abstraction
is a concrete session owned by either an in-process Hub or a same-host daemon.
Transport, terminal UI, remote authentication, and storage policy are outside
that abstraction.

## Public layers

### Hub and Client

`Hub` owns local PTYs. `Client` reaches a `Server` over a Unix socket. Both
provide the same `Start`, `Get`, and `List` signatures and return `*Session`.
The daemon transport is an implementation detail of the session handle.

### Session

Identity (`Name`, `CreatedAt`, and `Info`) is immutable and cached. Every method
that can touch a process, storage, or a network accepts `context.Context` and
returns an error. Lifecycle verbs are explicit: `Terminate` ends the process
and retains its record and output; `Delete` removes the process, record, and
output storage. `Close` is reserved for readers and client-owned resources.

The package owns the concrete handle. Consumers that need substitutes define
their own narrow interfaces at the point of use.

### Process specification

`ProcessSpec.Path` and `Args` are the primary form. The runtime never silently
passes them through a shell. `Shell` is an explicit opt-in and cannot be mixed
with `Path` or `Args`.

### Output

Raw output is an ordered, generational log. `OutputReader` supplies pull-based
backpressure and a cursor for its next unread byte. Cursors are opaque because
generation layout and retention are runtime concerns.

`Replay` represents rendered terminal state. `Checkpoint` is the only API that
atomically relates rendered state to raw output position.

## Ownership and concurrency

- The caller closes every `OutputReader` and owns any goroutine that reads it.
- Canceling a reader context or closing the reader unblocks a pending read.
- The runtime does not invoke output callbacks or create per-session watcher
  goroutines in caller space.
- Hub and server own PTY-copy goroutines until session termination or transfer.
- Archive format, compression, rotation triggers, and prune timing belong to
  the application.
- Managed daemon startup is opt-in through `ConnectManaged`; plain clients do
  not spawn processes.

## Protocol bounds

The public daemon protocol uses frames no larger than 1 MiB. Large raw output,
Replay, and Checkpoint replay data uses 64 KiB pull chunks. A peer cannot force
a single arbitrary allocation through a declared frame. Public Replay methods
still return a complete `[]byte`, so the configured VT budget remains the
upper-level memory control.

The exact envelope, raw-payload framing, request sequencing, stream state
machine, limits, and compatibility rules are frozen in [RFC 0004](0004-wire-protocol.md).

## Durability boundary

Client detach does not affect a daemon-owned session. A successful same-version
adoption preserves PTYs, VT state, and output cursors. A daemon crash, host
reboot, or move to another machine is outside the guarantee. Output files are
history, not process persistence.

## Compatibility

Protocol v1 begins at `1.0.0`. No v0 source API, wire method, cursor, output
offset, or migration snapshot is accepted. This keeps validation local and
prevents an untestable compatibility matrix from becoming part of the first
stable contract.
