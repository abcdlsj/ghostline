# ghostline

`ghostline` is an embeddable terminal engine for Go. It owns real
pseudo-terminals, keeps sessions running independently of attached clients,
stores raw output in append-only spools, and renders complete terminal replays
with [libghostty-vt](https://github.com/ghostty-org/ghostty).

It provides terminal mechanics rather than a transport or UI. Applications can
build a local multiplexer, remote shell service, development agent, browser
terminal, or their own session protocol on top.

## Capabilities

- Process-owned PTY sessions with input, resize, and exit reporting
- Ghostty-compatible VT state, scrollback, colors, alternate screens, and
  synchronized output
- Raw append-only output subscriptions with resumable byte offsets
- Atomic checkpoints pairing a full VT replay with its exact spool boundary
- Detached-mode terminal query responses for TUIs
- Local `Hub` and Unix-socket `Server`/`Client` with one `Session` API

## Requirements

- Go 1.25+
- Unix-like system (macOS, Linux, BSD)
- `libghostty-vt`; see [libghostty-vt](#libghostty-vt) below

## Quick start: local hub

```go
hub, err := ghostline.New(ghostline.Options{
	OutputDir:   "/var/lib/my-app/terminals",
	DefaultSize: ghostline.Size{Columns: 120, Rows: 36},
})
if err != nil {
	return err
}
defer hub.Close()

session, err := hub.Start(ctx, ghostline.SessionOptions{
	Name:        "build-shell",
	Directory:   "/path/to/worktree",
	Command:     "bash",
	Environment: []string{"MY_APP=1"},
})
if err != nil {
	return err
}

watcher, err := session.WatchOutput(ghostline.WatchOptions{
	OnOutput: func(data []byte) {
		_, _ = os.Stdout.Write(data)
	},
})
if err != nil {
	return err
}
defer watcher.Close()

_ = session.Input(ctx, []byte("go test ./...\r"))
_ = session.Resize(ctx, ghostline.Size{Columns: 100, Rows: 30})
```

`WatchOutput` starts at a raw spool offset. Its callback receives a borrowed
slice that is valid only until the callback returns; copy it if it must be
retained.

Child processes inherit the embedding process's environment;
`SessionOptions.Environment` overrides individual `KEY=value` entries.

## Quick start: server and client

Run the standalone daemon:

```sh
go run ./cmd/ghostline serve --socket /tmp/ghostline.sock
```

Embed the server, or connect from another process:

```go
client := ghostline.NewClient("/tmp/ghostline.sock")
if err := client.Check(ctx); err != nil {
	return err
}

session, err := client.Start(ctx, ghostline.SessionOptions{
	Name:      "remote-shell",
	Directory: "/srv/worktree",
	Command:   "bash",
})
if err != nil {
	return err
}
```

`Client.Start` returns a `Session` with the same methods as a local one,
including `Wait`, `Checkpoint`, and `Recover`. Clients and the server must
share the same filesystem because output watchers read the spool files
directly.

### Server bootstrap

`Connect` starts the server when the socket is missing:

```go
client, err := ghostline.Connect(ctx, ghostline.ConnectOptions{
	Socket: "/tmp/ghostline.sock",
})
if err != nil {
	return err
}
defer client.Close()
```

The default spawn command is `ghostline serve --socket <path>`. `Spawn` can
override it, with `{socket}` replaced by the socket path; `Env` and `Log`
configure the spawned process. `Ensure` pre-warms the server without an
operation. A spawn that exits before becoming ready is reported immediately,
including its output. Concurrent `Connect` calls are safe: the first server
to bind wins, and the other clients attach to it without owning it.

Recovery is lazy and restricted: read-only calls and `Start` may respawn and
retry once after a dead socket. `Input`, `Resize`, `Close`, `Remove`, and spool
maintenance are never retried automatically. `Close` stops only the server
this client spawned; connecting to an existing server is a no-op.

## Checkpoints

For a lossless reattach or window switch, pause the watcher and use an atomic
checkpoint:

```go
watcher.Pause()
checkpoint, err := session.Checkpoint(ctx)
if err == nil {
	err = watcher.SkipTo(checkpoint.Offset)
}
if err == nil {
	_, err = client.Write(checkpoint.Replay)
}
watcher.Resume()
```

Bytes below `Checkpoint.Offset` are represented by `Checkpoint.Replay`; bytes
written afterwards remain available to the resumed watcher.

## minimux example

`examples/minimux` is a small in-process terminal multiplexer built on the
public `Hub` and `Session` APIs:

```sh
cd examples/minimux && go run .
cd examples/minimux && go run . -- htop
```

It demonstrates multiple live windows, background TUI query responses,
terminal resizing, output subscriptions, and atomic VT replay on window
switches.

| Key | Action |
| --- | --- |
| `Ctrl-B c` | Create a shell window |
| `Ctrl-B n` | Switch to the next window |
| `Ctrl-B p` | Switch to the previous window |
| `Ctrl-B x` | Close the current window |
| `Ctrl-B q` | Quit and terminate all windows |
| `Ctrl-B Ctrl-B` | Send a literal `Ctrl-B` |

## libghostty-vt

The repository includes the Ghostty C headers and a prebuilt macOS arm64
library. Other platforms must build libghostty-vt from Ghostty source with Zig
0.15.2:

```sh
brew install zig@0.15
git clone https://github.com/ghostty-org/ghostty
cd ghostty
/opt/homebrew/opt/zig@0.15/bin/zig build \
  -Doptimize=ReleaseFast -Demit-lib-vt=true
```

Point the external linker and runtime loader at that build on platforms that do
not use the bundled macOS arm64 library:

```sh
export GHOSTTY_VT_DIR="$HOME/Workspace/gh/ghostty/zig-out"
export CGO_LDFLAGS="-L$GHOSTTY_VT_DIR/lib"
export DYLD_LIBRARY_PATH="$GHOSTTY_VT_DIR/lib${DYLD_LIBRARY_PATH:+:$DYLD_LIBRARY_PATH}" # macOS
# export LD_LIBRARY_PATH="$GHOSTTY_VT_DIR/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"   # Linux
```

Builds with `CGO_ENABLED=0` compile, but `Hub.Check` and `Start` return
`ErrUnavailable` because VT emulation requires libghostty-vt.

## Protocol and security

The server speaks JSON-lines over a Unix socket and creates the socket with
mode `0600`. It enforces an idle deadline, a message size limit, and a
concurrent connection cap. It is designed for same-machine, trusted callers;
there is no authentication. Do not expose the socket to untrusted users.

## Lifecycle

- `Start` rejects names that are already known; call `Remove` before reusing a
  name.
- `Close` terminates a session but keeps its record and spool for inspection.
- `Remove` deletes the in-memory record; spool files stay on disk.
- `Hub.Close` terminates every session.
- `Session.Wait` returns an `ExitError` with the exit code or signal. Context
  cancellation stops waiting but does not terminate the child.
- `Status` distinguishes a stopped session from a remote network failure;
  `Alive` is a best-effort convenience.
- Spool maintenance lives on `Session`: `Recover`, `TruncateSpool`,
  `ArchiveSpool`, and `RemoveSpool`.
- Use `errors.Is` with `ErrUnavailable`, `ErrClosed`, `ErrSessionExists`,
  `ErrSessionNotFound`, `ErrSessionClosed`, and `ErrInvalidSessionName`; error
  identity is preserved across the RPC boundary.

## Layout

- `hub.go` - session hub and start options
- `session.go` - local and remote session API
- `process.go` - PTY child lifecycle
- `spool.go` - append-only output watcher
- `query.go` - detached-mode terminal query responder
- `terminal.go` - libghostty-vt CGo wrapper
- `rpc.go`, `client.go`, `server.go` - Unix-socket protocol
- `cmd/ghostline` - standalone server command
- `examples/minimux` - runnable terminal multiplexer example
