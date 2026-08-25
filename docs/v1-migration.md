# Migrating to ghostline v1

ghostline v1 is a clean break for public clients and RPC methods. Upgrade
clients and daemon servers together. The final v0.8 daemon has one separate
admin-only handoff contract for Warren-coordinated upgrades; it is not a
source compatibility shim or a mixed v0/v1 public socket.

## API replacements

| v0 concept | v1 contract |
| --- | --- |
| provider-defined `Session` interface | concrete `*Session`; consumers define narrow interfaces |
| command string fields | `ProcessSpec{Path, Args}` or explicit `Shell(command)` |
| `Alive()` | `Status(ctx)` |
| `Done()` | `Wait(ctx)` |
| `Input(ctx, data)` | `WriteInput(ctx, data)` |
| screen `Snapshot` | `Replay(ctx)` |
| `WatchOutput` and spool callbacks | `Output(ctx, cursor)` and caller-owned read loop |
| bare spool offset | opaque generational `Cursor` |
| `Checkpoint.Offset` | `Checkpoint.Cursor` |
| session `Close` | `Terminate(ctx)` |
| session `Remove` | archive if needed, then `Delete(ctx)`; v1 also removes output storage |
| core archive/truncate helpers | `RotateOutput` and `PruneOutput`; archive policy stays outside |
| implicit managed `Connect` | `ConnectManaged` with `ManagedClientOptions` |

`Hub` and `Client` now share `Start`, `Get`, and `List` method shapes. All
operations that may perform I/O take a context and return an error. Code that
previously treated an empty value or `false` as a network result must handle
the error instead.

## Reattach ordering

Do not translate a watcher callback mechanically. A v1 consumer owns its
reader goroutine and must stop it before creating a checkpoint:

1. cancel or close the old reader;
2. wait for its goroutine;
3. call `Checkpoint`;
4. open `Output` at `Checkpoint.Cursor`;
5. write `Checkpoint.Replay`;
6. start reading raw output.

## v0.8 daemon handoff

Warren may first roll an older v0 daemon into the final v0.8 compatibility
bridge, then start v1 from its admin socket on the next restart. v1 accepts
only `handoffVersion == "ghostline-v0-to-v1-1"` with source protocol `0.8.0`.
It replays the v0 archived and live spool files into a new generation-one v1
output log, restores the VT snapshot, and creates fresh opaque v1 cursors.
The v0 spool path, size, and format are migration metadata only; a v0 byte
offset is never exposed as a v1 cursor.

The bridge remains same-host and all-or-nothing. Warren owns discovery, socket
switching, process startup, and source retirement. See
[`v0-compat-bridge.md`](v0-compat-bridge.md).

## Deployment cutover

A pre-0.8 v0 daemon cannot be adopted directly by a v1 daemon. Normalize it to
v0.8 first, or stop accepting new v0 sessions, let existing sessions finish or
terminate them deliberately, and start v1 with a fresh socket and output root.
Do not infer cursors from v0 spool sizes; the handoff always creates a new v1
generation and validation state.

Once every deployed component speaks `ProtocolVersion == "1.0.0"`, future
same-version daemon replacements can use native rolling adoption.
