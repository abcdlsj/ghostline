# Scrollback and output retention

ghostline keeps rendered terminal history and raw PTY output for different
purposes. They must not share one retention knob.

| Layer | Contents | Default | Owner |
| --- | --- | ---: | --- |
| VT scrollback | Rendered cells, styles, wraps, and terminal modes | 2 MiB logical budget | ghostline mechanism; caller configures budget |
| Raw output generations | Original PTY bytes and escape sequences | No automatic cap | caller retention policy |

## VT scrollback

`Options.VTScrollbackMaxBytes` sets the Hub default.
`SessionOptions.VTScrollbackMaxBytes` overrides it for one session. A zero
value inherits the 2 MiB package default.

libghostty applies the limit at its internal page granularity, so the value is
a logical budget, not an exact heap ceiling. Increasing it makes `Replay`,
`Checkpoint`, migration snapshots, and session memory larger. Daemon Replay
and Checkpoint data is chunked on the wire, but the public methods still return
the complete replay as `[]byte`; chunking does not remove its total memory or
encoding cost.

## Raw output generations

Each session owns one active segment and zero or more immutable completed
segments. A `Cursor` identifies a generation and byte offset without exposing
those fields as public API.

`RotateOutput` atomically completes the active generation and publishes a new
generation-boundary cursor. Readers cross completed generations in order.
`PruneOutput` removes generations strictly before such a boundary.

An already-open reader pins its current file descriptor and may drain it after
pruning. A new reader at a pruned generation receives `ErrCursorExpired`.
This distinction avoids corrupting in-flight consumers while making retention
state explicit to reconnecting consumers.

The core does not gzip, archive, count, or schedule retention. An application
can rotate by size or time, copy completed segments into its own archive, wait
for consumer acknowledgements, and then prune. Those choices depend on the
application's durability and storage promises and do not belong in the
terminal runtime.

## Checkpoint relationship

`Checkpoint` captures a terminal replay and the current raw-output cursor
under the same output lock. The replay represents terminal state through that
cursor. It does not guarantee that every historic raw byte is still present,
and raw segments cannot reconstruct VT history already discarded from the
emulator without replaying from a complete earlier anchor.

For reattachment, stop the old reader, create a checkpoint, open a new reader
at its cursor, write the replay, and then start the reader. This order avoids
both duplicate bytes and a gap between rendered state and live output.

## Practical policy

Start with the 2 MiB VT budget for interactive sessions. Measure checkpoint
latency and memory before increasing it. Choose raw-output rotation and pruning
from explicit requirements such as reconnect window, archive cost, and the
slowest consumer cursor. Do not equate a raw-byte budget with a rendered-cell
budget.
