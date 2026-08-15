# ghostline

Server-side PTY sessions with a Ghostty VT emulator.

`ghostline` is the terminal runtime layer behind
[Warren](https://github.com/abcdlsj/warren): it owns one pseudo-terminal per
session, streams raw PTY bytes to an append-only spool, and renders screen
snapshots with [libghostty-vt](https://github.com/ghostty-org/ghostty) — the
same terminal core the Ghostty client uses. Snapshots therefore preserve
colors, alt-screen, DEC 2026, and scrollback exactly as the client would
render them, at any requested size.

It is designed to be embedded (a tmux-free runtime for headless daemons) and
usable on its own (create sessions, feed input, render snapshots, kill).

## Requirements

- Go 1.25+
- `libghostty-vt` built from the Ghostty source with Zig 0.15.2:

```sh
brew install zig@0.15
git clone https://github.com/ghostty-org/ghostty
cd ghostty
/opt/homebrew/opt/zig@0.15/bin/zig build -Doptimize=ReleaseFast -Demit-lib-vt=true
```

The CGo wrapper expects `include` and `lib` under `GHOSTTY_VT_DIR`
(default `$HOME/Workspace/gh/ghostty/zig-out`).

## Usage

```go
import "github.com/abcdlsj/ghostline"

manager := ghostline.NewPTY(outputDir)
err := manager.Create(ctx, "ghost_abc", "/path/to/worktree", "codex")
manager.Input(ctx, "ghost_abc", []byte("hello\r"))
snapshot, err := manager.Capture(ctx, "ghost_abc")
manager.Resize(ctx, "ghost_abc", 100, 30)
manager.Kill(ctx, "ghost_abc")
```

## Layout

- `pty.go` — PTY session lifecycle and spool management
- `ghosttyvt.go` — CGo wrapper around libghostty-vt (`VTTerminal`)
- `spool.go` — append-only output spool watcher
- `query.go` — detached-mode terminal query responder (DA/DSR/OSC/kitty)
