# Scrollback and Output Retention

ghostline keeps two different forms of terminal history. They have different
owners, units, and recovery behavior.

## Retention Layers

| Layer | Contents | Default in this repository | Purpose |
| --- | --- | --- | --- |
| VT scrollback | Rendered cells, styles, wraps, and terminal state | 2 MiB logical bytes per terminal | Build a screen snapshot with recent visible history |
| Raw spool | Original PTY output bytes, including escape sequences | No automatic cap in the core Hub | Exact replay, checkpoints, and consumer-defined recovery |
| Spool watcher threshold | A callback threshold for a watched spool | 64 MiB when `WatchOptions.MaxBytes` is zero | Tell the consumer that it should compact or rotate |
| Warren live spool | Raw PTY output retained on disk | 8 MiB | Bound the raw recovery window before archive/truncate and reanchor |
| Warren output ring | Recent raw output retained in memory | 8 MiB | Serve fast reconnects before falling back to spool recovery |

The core library does not truncate a spool when the watcher threshold is
crossed. The `OnOverflow` callback decides what to do. Warren sets its live
spool cap to 8 MiB, archives the current spool, truncates it, and reanchors
clients with a screen snapshot.

A VT snapshot cannot restore history that the VT emulator has already pruned.
The raw spool can still contain that output, but recovering it requires replaying
the raw bytes or serving them through an explicit history path. Increasing the
spool cap alone does not increase the history carried by a snapshot.

## Configuration

The default is suitable for an interactive terminal while keeping reanchor and
rolling-upgrade snapshots bounded:

```go
hub, err := ghostline.New(ghostline.Options{
	OutputDir:            "/var/lib/my-app/terminals",
	VTScrollbackMaxBytes: 2 << 20,
})
```

An individual session can override the Hub default. Zero means inherit the Hub
setting, and a direct `NewVTTerminalWithOptions` call uses the package default
when its option is zero.

```go
session, err := hub.Start(ctx, ghostline.SessionOptions{
	Name:                 "build-shell",
	VTScrollbackMaxBytes: 4 << 20,
})
```

The standalone server accepts the same default through
`--vt-scrollback-max-bytes`. The client-to-server create request also carries
the per-session value.

libghostty enforces the byte budget at page granularity. The configured value
is therefore an estimate: the actual retained allocation can be larger, and a
page is roughly 400 KiB in the current implementation. A larger limit also
increases the work and payload size of snapshots.

## Other Defaults

The following are common documented defaults. They are configurable and can
change across versions; they are included as reference points rather than
protocol guarantees.

| Runtime or terminal | Default history | Unit |
| --- | ---: | --- |
| tmux | 2,000 | lines per window |
| GNU Screen | 100 | lines per window |
| Zellij | 10,000 | lines |
| Alacritty | 10,000 | lines |
| WezTerm | 3,500 | lines |
| kitty | 2,000 | lines |

The kernel PTY and common PTY libraries such as `creack/pty` and `node-pty`
do not provide user-visible scrollback. They transport bytes; the terminal
emulator or multiplexer owns history.

tmux documents its default through `history-limit`; GNU Screen documents its
default through `defscrollback`. The other terminal applications expose
equivalent line-count settings in their configuration files. These line-based
buffers are not directly comparable to ghostline's rendered byte budget.

## Recommended Values

For an interactive Desktop session, use:

- VT scrollback: 2 MiB by default, with 1-4 MiB as a practical range.
- Warren live spool: 8 MiB as the raw recovery window.
- Larger VT limits only for log-heavy sessions that need more history after a
  snapshot reanchor; benchmark snapshot latency and per-session memory first.

Do not make the VT budget equal to the raw spool by default. The spool is an
exact byte stream, while a snapshot is rendered terminal state and may be much
more expensive to encode. The output protocol can split a large snapshot into
8 MiB frames, but splitting does not remove the memory or latency cost.

References:

- [tmux manual](https://man7.org/linux/man-pages/man1/tmux.1.html)
- [GNU Screen manual](https://www.gnu.org/software/screen/manual/screen.html)
- [Zellij configuration](https://zellij.dev/documentation/options.html)
- [Alacritty configuration](https://alacritty.org/config-alacritty.html)
- [WezTerm scrollback](https://wezterm.org/config/lua/config/scrollback_lines.html)
- [kitty configuration](https://sw.kovidgoyal.net/kitty/conf/)
