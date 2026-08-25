# ghostline

`ghostline` is a local-first terminal session runtime for Go. It owns real
pseudo-terminals, keeps child processes independent from clients, maintains a
Ghostty-compatible terminal model, and exposes resumable raw output.

The package provides terminal mechanics. It is not a terminal UI, a remote
shell protocol, a cross-host migration system, or a reboot-persistent process
manager.

## v1 capabilities

- In-process sessions through `Hub`, or same-host process separation through
  `Server` and `Client`.
- One concrete `*Session` API for local and daemon-owned sessions.
- Argv-first process startup. Shell evaluation is explicit through `Shell`.
- Context-aware input, resize, wait, status, metadata, replay, output, and
  lifecycle operations.
- Immutable output segments addressed by opaque, comparable `Cursor` values.
- Bounded `OutputReader` streams with cancellation and natural backpressure.
- Atomic checkpoints that pair a VT replay with the first raw output byte not
  represented by that replay.
- Same-version, all-or-nothing daemon upgrades that adopt live PTY file
  descriptors, VT state, and output generations.

v1 is intentionally incompatible with every v0 public API and wire method.
The final v0.x daemon exposes a separate, explicit handoff contract for
Warren-coordinated upgrades; it is not a mixed-protocol compatibility mode.
See [the v1 migration note](docs/v1-migration.md).

## Requirements and platform status

- Go 1.25 or newer.
- macOS 13 or newer on amd64 or arm64, with CGo enabled.
- Linux with glibc 2.31 or newer on amd64 or arm64, with CGo enabled.

Both supported families statically embed `libghostty-vt`; applications do not
need to install or deploy a Ghostty dynamic library. FreeBSD is compile-checked
with CGo disabled but does not have a working VT runtime. Windows is not
supported because ghostline requires Unix PTYs, Unix sockets, file-descriptor
transfer, and process-group signals.

The bundled libraries are built from pinned Ghostty commit
`5851d98615187d85052e41042bcf66e0ccec11d4`. See
[the third-party artifact manifest](third_party/README.md) for build commands,
targets, checksums, and license information.

For high-density daemon use, set a realistic `VTScrollbackMaxBytes` budget and
an explicit `ServerMaxClientConnections` limit. The default connection limit is
1,024. Each live Output, Replay, or Checkpoint stream holds one server socket
connection; the daemon is not a multiplexed stream transport. Clients can read
the configured limit from `Client.VersionInfo`.

## Local sessions

```go
hub, err := ghostline.New(ghostline.Options{
	OutputDir:   "/var/lib/my-app/ghostline",
	DefaultSize: ghostline.Size{Columns: 120, Rows: 36},
})
if err != nil {
	return err
}
defer hub.Close()

session, err := hub.Start(ctx, ghostline.SessionOptions{
	Name: "build",
	Process: ghostline.ProcessSpec{
		Path:        "go",
		Args:        []string{"test", "./..."},
		Directory:   "/path/to/worktree",
		Environment: []string{"CI=1"},
	},
})
if err != nil {
	return err
}

output, err := session.Output(ctx, ghostline.Cursor{})
if err != nil {
	return err
}
defer output.Close()

_, err = io.Copy(os.Stdout, output)
```

A zero `ProcessSpec` starts `$SHELL`, falling back to `sh`. Use
`ghostline.Shell("make test && make lint")` only when shell parsing is wanted.
`Environment` overrides matching inherited variables. ghostline supplies
`TERM=xterm-256color` when the resulting environment has no non-empty `TERM`.

## Output and checkpoints

`Output(ctx, cursor)` returns raw PTY bytes in order. A zero cursor starts at
the earliest retained generation. Each `Read` returns at most the size of the
caller's buffer; daemon reads request at most 64 KiB per round trip. Closing
the reader or canceling its context unblocks a pending read.

`Cursor` fields are deliberately private. Store `Cursor.String()` or use its
text/JSON marshaling methods. `ParseCursor` accepts the stable v1 text form.
After retention removes a generation, opening a new reader at one of its
cursors returns `ErrCursorExpired`. A reader that already pinned the segment
may finish draining it.

For a lossless window switch or client reattach:

1. Stop and wait for the current output-reading goroutine.
2. Call `Checkpoint(ctx)`.
3. Open a new reader at `checkpoint.Cursor`.
4. Write `checkpoint.Replay` to the destination.
5. Start reading from the new reader.

The checkpoint lock orders the VT replay and cursor atomically. Starting the
new reader only after writing the replay prevents raw bytes from appearing
before the reconstructed screen.

## Output retention

The core exposes mechanism, not retention policy:

```go
boundary, err := session.RotateOutput(ctx)
if err != nil {
	return err
}
// Archive or account for completed generations in application policy.
if err := session.PruneOutput(ctx, boundary); err != nil {
	return err
}
```

Rotation completes the active generation and creates a new one. Pruning only
accepts a generation-boundary cursor returned by rotation. The core does not
compress segments, choose archive counts, or prune automatically. VT
scrollback and raw output retention are independent; see
[scrollback and output retention](docs/scrollback.md).

## Daemon client

Run the server:

```sh
go run ./cmd/ghostline serve --socket /tmp/ghostline.sock
```

Use `NewClient` when the caller or a service manager owns that process:

```go
client := ghostline.NewClient("/tmp/ghostline.sock")
if err := client.Check(ctx); err != nil {
	return err
}
```

Use the explicitly managed constructor when the client should spawn a missing
server and lazily restore service after a transport failure:

```go
client, err := ghostline.ConnectManaged(ctx, ghostline.ManagedClientOptions{
	Socket: "/tmp/ghostline.sock",
	Spawn:  []string{"ghostline", "serve", "--socket", "{socket}"},
})
if err != nil {
	return err
}
defer client.Close()
```

`ConnectManaged` keeps at most the latest 64 KiB of startup diagnostics.
Read-only operations and `Start` may bootstrap and retry once after a transport
failure. Input, resize, termination, deletion, rotation, and pruning are never
retried because repeating them can change meaning. `Client.Close` stops only a
server spawned by that client.

`Hub` and `Client` expose matching `Start`, `Get`, and `List` methods. Daemon
output, replay, and checkpoints travel over the socket; clients never open
server-side output files.

## Lifecycle and durability

- `Wait` reports `*ExitError`. Canceling its context stops waiting, not the
  child.
- `Status` distinguishes process state from transport failure.
- `Terminate` ends the process tree but retains the session record and output.
- `Delete` ends the process tree and removes both the session record and its
  output storage. Archive required output before deletion.
- `Hub.Close` terminates all local sessions. `Server.Shutdown` does the same
  for daemon-owned sessions.
- Foreground process metadata is opt-in through `ProbeForeground`.

Sessions survive client detach and a successful same-version rolling daemon
upgrade. They do not survive daemon crashes that lose PTY masters, host
reboots, or cross-machine moves. Raw output is written to files, but those
files cannot resurrect a dead process or reconstruct an unretained VT state.

## Rolling daemon upgrades

`Server.Adopt` and `ghostline serve --adopt-from` transfer a complete batch
from an existing server's `<socket>.admin` endpoint. Both sides must advertise
exactly `ProtocolVersion == "1.0.0"`; protocol mismatch is rejected before any
session is prepared. A successful batch transfers native VT state, live PTY
file descriptors, output directories, and the active generation. There is no
implicit v0 replay bridge in this native path. The final v0.8 compatibility
daemon can be handed off separately through
[`docs/v0-compat-bridge.md`](docs/v0-compat-bridge.md); that path rebuilds a
fresh v1 output generation and never reuses a v0 byte offset as a cursor.

See [RFC 0002](docs/rfc/0002-serve-rolling-upgrade.md) for ordering and failure
semantics.

## Protocol and security

The daemon uses the v1 bounded JSON-envelope protocol over mode `0600` Unix
sockets. Binary input/output is an exact-length raw payload after the JSON
header; it is not JSON/base64. Headers and payloads are limited to 1 MiB, and
raw output, Replay, and Checkpoint replay use 64 KiB pull-stream chunks. See
[RFC 0004](docs/rfc/0004-wire-protocol.md) for framing, IDs, state, and
extension rules. Use `errors.Is(err, ErrFrameTooLarge)` for a frame limit
violation.

The public and admin sockets assume trusted same-user, same-host callers. They
have no remote authentication or encryption. Do not expose them through a TCP
proxy or to another trust domain.

## minimux example

The independent module in `examples/minimux` demonstrates multiple windows,
background terminal-query responses, output readers, and checkpoint-safe
switching:

```sh
cd examples/minimux
go run .
go run . -- htop
```

Keys are `Ctrl-B c` (create), `Ctrl-B n/p` (switch), `Ctrl-B x` (delete), and
`Ctrl-B q` (quit).

## libghostty-vt

The repository includes headers, a macOS 13+ universal static archive, and
Linux glibc 2.31+ static archives for amd64 and arm64. The package selects and
links the matching archive through CGo. Builds with CGo disabled, or builds on
an unsupported OS or architecture, compile with the unavailable backend;
`Hub.Check` and `Start` then return `ErrUnavailable`.

## Design documents

- [RFC 0001: explicit managed client lifecycle](docs/rfc/0001-server-lifecycle.md)
- [RFC 0002: same-version rolling daemon upgrade](docs/rfc/0002-serve-rolling-upgrade.md)
- [RFC 0003: v1 architecture and ownership](docs/rfc/0003-v1-architecture.md)
- [v1 migration note](docs/v1-migration.md)
- [v1.0 release checklist and platform matrix](docs/release-checklist.md)
