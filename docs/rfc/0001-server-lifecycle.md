# RFC 0001: Server lifecycle owned by ghostline

- Status: Accepted
- Date: 2026-08-16
- Area: Server / Client lifecycle

## Summary

Move the ghostline server process lifecycle into ghostline itself. A
`Client` becomes able to self-bootstrap its server (check the socket, spawn
the server when missing, wait for readiness, reconnect after a crash), so
embedding applications only provide *how to execute* the server, never *how
to keep it alive*.

## Problem

Today the server process is spawned by the embedding application. Warren's
daemon, for example, checks the socket once at startup and spawns its own
binary with `--ghostline-serve`; it never notices a server crash afterwards.

This has three consequences:

1. **The lifecycle logic lives in the wrong layer.** Every embedder that
   wants a standalone server must reimplement socket probing, spawning, and
   waiting. ghostline knows its own protocol and failure modes; its clients
   should not depend on the embedder for basic availability.
2. **No self-healing.** A server that dies mid-run leaves the daemon
   connected to a dead socket until the daemon itself restarts.
3. **Inconsistent behavior across consumers.** minimux, Warren, and future
   embedders would each grow a different variant of the same bootstrap.

## Goals

- `ghostline.Connect` returns a working `Client`, spawning the server when
  the socket is missing.
- The client transparently recovers from a dead server: operations that fail
  because the socket is gone re-bootstrap once and retry.
- The spawn command is caller-provided, with a sensible default.
- The API reads like the rest of the std-lib style surface: `Connect(ctx,
  ConnectOptions) (*Client, error)`.

## Non-goals

- **Resurrecting crashed sessions.** A server crash closes its PTY masters;
  child processes exit and cannot be reattached. This RFC only restores the
  *service*, not the processes. Rebuilding sessions from application state is
  an embedder concern.
- **Managed daemons (launchd / systemd).** Running the server under a service
  manager is a deployment option and stays out of the library.
- **Background watchdog goroutines.** Recovery is lazy: the client notices a
  dead socket on its next operation. This keeps the design simple and avoids
  hidden goroutines.

## Design

### Connect

```go
type ConnectOptions struct {
    // Socket is the Unix socket path the server listens on.
    Socket string
    // Spawn is the command used to start the server when the socket is
    // missing. Arguments may contain the placeholder {socket}, replaced by
    // Socket. Empty uses ["ghostline", "serve", "--socket", socket].
    Spawn []string
    // Env overrides the spawned server's environment.
    Env []string
    // ReadyTimeout bounds how long Connect waits for the socket after
    // spawning. Zero uses 5s.
    ReadyTimeout time.Duration
    // Log receives the spawned server's stdout/stderr. Empty discards it.
    Log io.Writer
}

func Connect(ctx context.Context, options ConnectOptions) (*Client, error)
```

Behavior:

1. `Ping(Socket)` succeeds → return `NewClient(Socket)`.
2. Socket missing → run `Spawn` detached (new session, no terminal), wait up
   to `ReadyTimeout` for the socket, then return the client.
3. Spawn fails or the socket never appears → return an error that includes
   the spawn output. A spawn that exits before becoming ready is reported
   immediately instead of waiting out `ReadyTimeout`.

Concurrent callers are safe: Unix `listen` is exclusive, so only one spawn
wins; losers simply connect to the winner's socket and their `Close` is a
no-op because they do not own the live server.

### Lazy recovery

`Client.call` maps a connection failure to "server may be down" for
idempotent operations only, then:

1. `Ping(Socket)`. If it still answers, the failure was transient — return
   the original error.
2. Otherwise run the same spawn/ready sequence as `Connect` and retry the
   operation once.
3. If the retry fails, return the retry error.

`Input`, `Resize`, `Close`, `Remove`, and spool maintenance are never
auto-retried because a retry could duplicate input or affect a new server's
state.

`Client.Close` stops only the server that `Connect` spawned; clients that
connected to an existing server are a no-op.

After a server crash the client's remote `Session` handles refer to sessions
the new server does not know; their operations return `ErrSessionNotFound`.
Embedders decide whether to rebuild those sessions from their own state.

## Alternatives considered

- **Embedder-owned lifecycle (status quo plus a watchdog in Warren).** Works
  for Warren but every other consumer repeats it; rejected because the
  boundary stays wrong.
- **Service manager (launchd/systemd) with KeepAlive.** Reasonable for
  production deployments, but deployment-specific; left to embedders as an
  option, not built into the library.
- **Supervisor process holding PTY masters.** Would allow true process
  survival across server crashes, but adds a second daemon layer and still
  cannot reattach a dead PTY pair. Rejected as over-engineering.

## Migration

- Add `ConnectOptions`, `Connect`, and the recovery path to the `Client`
  without changing existing `Server` / `Client` APIs.
- Warren replaces its `ensureGhostlineServer` with
  `ghostline.Connect(ctx, ghostline.ConnectOptions{Socket: socket, Spawn:
  []string{executable, "--ghostline-serve", "--ghostline-socket", socket}})`.
- The `ghostline serve` CLI keeps working for manual and service-manager
  deployments.

## Resolved questions

- **Default `Spawn`.** The default stays `ghostline serve --socket <path>`
  for development and CLI installs. Embedders that ship a different binary
  pass an explicit `Spawn` (as Warren does), so relying on `PATH` is always
  an embedder choice.
- **Crash-loop protection.** Recovery is lazy and bounded: each operation
  respawns at most once, and a spawn that exits before becoming ready is
  reported immediately with its output. A server that hangs without binding
  still fails at `ReadyTimeout`. There is no background watchdog and no
  additional backoff.
- **Explicit `Ensure`.** Implemented: `Client.Ensure(ctx)` pre-warms the
  server without an operation and is safe to call at any time.
