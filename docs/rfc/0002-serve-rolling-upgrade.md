# RFC 0002: Same-version rolling daemon upgrade

- Status: Accepted for v1
- Date: 2026-08-24
- Area: Daemon lifecycle and session adoption

## Decision

A new ghostline server may adopt every session from an old same-host server
without disconnecting child processes. Adoption prefers native state, but a
session whose native state cannot be encoded or decoded falls back to an ANSI
screen replay or a blank terminal while retaining its PTY. The source batch is
same-protocol only.

Native rolling adoption remains same-version only: a source whose advertised
protocol is not exactly `ProtocolVersion` is rejected by that path before any
session is prepared. The final v0.8 daemon has a separate, explicit
`ghostline-v0-to-v1-1` handoff contract; it is documented in
`docs/v0-compat-bridge.md` and never mixed into this native protocol.

## Scope

Adoption preserves:

- live PTY master file descriptors through `SCM_RIGHTS`;
- encoded libghostty VT state;
- immutable session identity and terminal size;
- the output directory and active output generation;
- stopped status, including a known exit result when available.

Adoption is same-host only. It does not survive host reboot, move a process to
another kernel, or make a daemon crash recoverable.

## Protocol

The source server exposes a mode `0600` admin Unix socket at
`<public-socket>.admin`. The adopting server uses these requests:

- `list`: freeze the source inventory and return the protocol version and
  session metadata;
- `adopt`: stabilize one session and transfer a live PTY descriptor;
- `snapshot`: return native VT state and an optional bounded ANSI replay;
- `commit` or `abort`: resolve the complete prepared batch;
- `exit`: request source retirement after commit.

Clients never connect to the admin socket.

## Ordering

1. The destination sends `list`. A protocol mismatch ends the attempt.
2. The source freezes hub-wide creation and deletion for the batch.
3. Each session stops its output reader at a stable boundary. Output already
   readable from the PTY is incorporated into both the VT and active output
   segment.
4. The source transfers metadata, the PTY descriptor when live, and native VT
   state plus a bounded ANSI replay. The destination opens the transferred
   output directory at the same generation when possible.
5. Only after every session is prepared does the destination send `commit`.
6. The source releases ownership. The destination starts reading transferred
   PTYs, including bytes buffered during the pause.
7. The destination starts serving its public socket and requests source exit.

Checkpoint cursors remain usable across a successful adoption because the
generation and segment files move with the session. No bare file offset is
interpreted as a durable position.

## Failure semantics

- A per-session preparation failure is aborted by name, leaving the migration
  batch frozen so later sessions can still be attempted.
- Native snapshot failures are recovered with the ANSI replay or a blank VT.
  If output storage cannot be opened, the destination creates a fresh output
  log and accepts loss of historical output.
- If any session remains unrecoverable, the destination aborts all prepared
  sessions before commit; the source resumes unchanged and remains the owner.
- Commit preflights all migration tickets before resolving any one of them.
- A child that exits during preparation makes its ticket unstable and aborts
  the batch.
- Source retirement is best-effort after commit. Failure to receive the exit
  confirmation does not roll back ownership already transferred.
- The destination does not expose a partially adopted inventory.

These rules prevent split ownership. They do not make process state durable
against source crashes during the protocol.

## Operational entry point

The standalone command adopts before binding its public socket:

```sh
ghostline serve \
  --socket /tmp/ghostline-new.sock \
  --adopt-from /tmp/ghostline-old.sock.admin
```

The coordinator switches clients only after adoption succeeds.
