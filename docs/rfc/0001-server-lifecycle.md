# RFC 0001: Explicit managed client lifecycle

- Status: Accepted for v1
- Date: 2026-08-24
- Area: Daemon client lifecycle

## Decision

ghostline exposes two visibly different client construction paths:

```go
func NewClient(socketPath string) *Client

type ManagedClientOptions struct {
	Socket       string
	Spawn        []string
	Env          []string
	ReadyTimeout time.Duration
	Log          io.Writer
}

func ConnectManaged(ctx context.Context, options ManagedClientOptions) (*Client, error)
```

`NewClient` is a plain endpoint handle. It never starts a process and has no
hidden lifecycle policy. `ConnectManaged` may attach to an existing daemon or
spawn one. The word “Managed” is required because process ownership, retries,
and `Close` behavior are material side effects.

There is no compatibility alias named `Connect` in v1.

## Managed behavior

`ConnectManaged` follows this sequence:

1. Validate the socket path and spawn environment.
2. Return immediately if the socket already accepts connections.
3. Start the configured command, replacing `{socket}` in each argument.
4. Wait until the socket is ready, the command exits, the context is canceled,
   or `ReadyTimeout` expires.
5. Retain at most the newest 64 KiB of startup stdout and stderr for an error.

An empty spawn command uses `ghostline serve --socket <socket>`. The caller
owns the risk of relying on `PATH`.

Concurrent managed connections rely on exclusive Unix socket binding. A
losing spawn may exit after another server binds. Its client attaches to the
winner and does not claim process ownership.

## Retry boundary

Managed clients may bootstrap and retry once after a socket transport failure
for operations that are safe to repeat:

- `Check`, version queries, `Get`, `List`, `Status`, `Metadata`, `Replay`,
  `Checkpoint`, and `AtomicState`;
- `Start`, because unique session names make duplicate creation observable as
  `ErrSessionExists`.

The client does not retry input, resize, termination, deletion, output
rotation, or pruning. Repeating those operations against a replacement daemon
could duplicate input or mutate unrelated new state.

Recovery restores service availability only. A daemon crash closes its PTY
masters; old `Session` handles then refer to records the replacement daemon
does not own.

## Ownership

`Client.Close` stops only the process successfully started and still owned by
that managed client. It is a no-op for `NewClient` and for managed clients that
attached to an existing server.

The library does not run a watchdog. Recovery happens only when the caller
performs an operation or explicitly calls `Ensure`.
