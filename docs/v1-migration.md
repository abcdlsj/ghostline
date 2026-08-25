# Migrating to ghostline v1

ghostline v1 is a clean break. Upgrade clients and daemon servers together.
There is no source compatibility shim, wire negotiation with v0, spool-offset
adapter, or rolling-upgrade bridge from a v0 daemon.

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

## Deployment cutover

A v0 daemon cannot be adopted by a v1 daemon. Stop accepting new v0 sessions,
let existing sessions finish or terminate them deliberately, stop the v0
server, and start v1 with a fresh socket and output root. Do not point v1 at
old spool files and infer cursors from their sizes; a v1 cursor includes a
generation and validation state that v0 never recorded.

Once every deployed component speaks `ProtocolVersion == "1.0.0"`, future
same-version daemon replacements can use native rolling adoption.
