# ghostline

`ghostline` is an embeddable server-side terminal engine for Go. It owns real
pseudo-terminals, keeps sessions running independently of attached clients,
stores raw output in append-only spools, and renders complete terminal replays
with [libghostty-vt](https://github.com/ghostty-org/ghostty).

It provides terminal mechanics rather than a transport or UI. Applications can
build a local multiplexer, remote shell service, development agent, browser
terminal, or their own session protocol on top. [Warren](https://github.com/abcdlsj/warren)
is one consumer, not a required integration model.

## Capabilities

- Multiple process-owned PTY sessions with input and resize support
- Ghostty-compatible VT state, scrollback, colors, alternate screens, and
  synchronized output
- Raw append-only output subscriptions with resumable byte offsets
- Atomic checkpoints that pair a full VT replay with its exact spool boundary
- Detached-mode terminal query responses for TUIs
- Structured `Manager` and `Session` APIs plus the original name-based `PTY`
  compatibility API

Sessions outlive client connections, but they remain children of the embedding
process. They do not survive that process exiting. A separate long-running
daemon can provide persistence across client restarts.

## Quick Start

```go
manager, err := ghostline.New(ghostline.Options{
    OutputDir: "/var/lib/my-app/terminals",
    DefaultSize: ghostline.Size{Columns: 120, Rows: 36},
})
if err != nil {
    return err
}
defer manager.Close()

session, err := manager.Start(ctx, ghostline.SessionOptions{
    Name:      "build-shell",
    Directory: "/path/to/worktree",
    Command:   "bash",
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
slice that is valid only until the callback returns. Copy it if it must be
retained.

For a lossless client reattach or window switch, pause its watcher and use an
atomic checkpoint:

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

## minimux Example

`examples/minimux` is a small in-process terminal multiplexer built entirely on
the public `Manager` and `Session` APIs:

```sh
go run ./examples/minimux
go run ./examples/minimux -- htop
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

`minimux` is intentionally process-local and ephemeral; it is an API example,
not a replacement for tmux's persistent server and client protocol.

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

Builds with `CGO_ENABLED=0` can import and compile ghostline, but `Manager.Check`
and session creation return `ErrUnavailable` because VT emulation requires
libghostty-vt.

## Lifecycle

- Call `Manager.Close` to terminate all sessions and release native VT state.
- Call `Session.Close` to terminate one session. It is idempotent per handle.
- Canceling `Session.Wait` stops waiting; it does not terminate the child.
- Close every `SpoolWatcher` created by `WatchOutput`.
- Spools and metadata remain after session close for recovery or diagnostics.
  The compatibility API exposes `RemoveSpool` when the application is ready to
  delete them.
- Use `errors.Is` with `ErrClosed`, `ErrSessionExists`, `ErrSessionNotFound`,
  `ErrSessionClosed`, `ErrInvalidSessionName`, and `ErrUnavailable`.

The legacy `NewPTY` and name-based methods remain available for existing
runtime adapters.

## Layout

- `session.go` - high-level manager, session, subscription, and checkpoint API
- `pty.go` - PTY process lifecycle and compatibility API
- `ghosttyvt.go` - CGo wrapper around libghostty-vt
- `spool.go` - append-only output spool watcher
- `query.go` - detached-mode terminal query responder
- `examples/minimux` - runnable terminal multiplexer example
