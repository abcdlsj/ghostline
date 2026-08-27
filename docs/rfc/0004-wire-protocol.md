# RFC 0004: v1 daemon wire protocol

- Status: Accepted
- Date: 2026-08-25
- Area: Daemon transport

## Scope

This RFC defines the public `Client`/`Server` protocol on the Unix socket. It
is deliberately separate from the rolling-upgrade admin protocol in RFC 0002.
The public protocol is same-host, same-user, request/response, and pull-based;
it is not a remote authentication or multiplexed message protocol.

## Versions

Three versions are independent:

- `ProtocolVersion` (`1.0.0`) is the semantic RPC/API contract advertised by
  `version` and checked during managed daemon use.
- `wire` (`1`) is the envelope and byte-framing version. A peer MUST reject an
  unsupported value with `ErrProtocolMismatch`/`protocol_mismatch` before
  dispatching a method.
- The VT snapshot format version is owned by the VT implementation and is not
  inferred from either RPC version.

Changing one does not silently change the others. The v1 public socket has no
v0 compatibility path. A separate admin-only v0.8 handoff contract is outside
this wire protocol and is accepted only by the migration coordinator.

## Envelope and framing

Each request and response starts with one UTF-8 JSON object terminated by `\n`.
Unknown JSON members MUST be ignored. Required members and types are strict.
The JSON line is bounded by `MaxHeaderBytes` (currently 1 MiB). A line is
followed immediately by exactly `payloadBytes` raw bytes, without base64,
padding, or another delimiter. `payloadBytes` is omitted or zero when there is
no payload and is bounded by `MaxPayloadBytes` (currently 1 MiB).

Request envelope:

```json
{"wire":1,"id":1,"method":"writeInput","params":{"name":"demo"},"payloadBytes":3}
```

Response envelope:

```json
{"wire":1,"id":1,"result":{"cursor":{"generation":1,"offset":3}},"payloadBytes":3}
```

`result` and `error` are mutually exclusive. An error response MUST have no
result and no payload. A response with a declared payload MUST be followed by
that many bytes; a short read is a transport/protocol failure and MUST NOT be
treated as EOF. A peer MUST reject negative or over-limit lengths. A payload
that a method does not define is an error; it MUST never be silently ignored.

`MaxChunkBytes` (currently 64 KiB) limits every stream chunk, even though the
overall payload limit is larger. Implementations MUST bound allocations from
peer-declared lengths by these limits.

## IDs and sequencing

`id` is a positive request identifier. The response MUST echo it exactly. `-1`
is reserved for a terminal transport/envelope error emitted before a valid
request identifier can be established. Clients MUST reject an unexpected ID.

A connection is strictly serial: one request is written, one matching response
is read, then the next request may be sent. There is no pipelining, concurrent
in-flight request, or response reordering in v1. Stream readers serialize their
own requests with a mutex. Future pipelining requires a new wire capability and
an explicit state-machine extension; it cannot be inferred from an unknown
field.

## Capabilities and limits

`version` returns the semantic version, a capability-name list, and
`ProtocolLimits`. It also returns `maxClientConnections`, the daemon's active
client-socket limit. Each long-lived Output, Replay, or Checkpoint stream counts
against that limit. Current capabilities are:

- `raw-payload-v1`: exact-length raw payload framing is supported.
- `pull-stream-v1`: stream open/read/close state machines below are supported.
- `atomic-state-v1`: the `state.atomic` blob method returns a complete opaque
  VT state paired with an output cursor.

Unknown capability names are ignored. A client MUST require the capabilities it
needs and MUST honor the peer's advertised minimum limits; it MUST NOT assume a
larger chunk or payload than advertised.

## Ordinary calls

Ordinary methods use one request and one response. `writeInput` is the only
ordinary method accepting a request raw payload in v1; its `params` identify the
session. Other ordinary methods reject request payloads. Ordinary responses are
JSON-only and clients reject an unexpected response payload.

## Pull-stream state machines

Replay, Checkpoint, and Atomic State use a blob stream; Output uses an output
stream. Both follow the same states:

```text
Open -> Ready -> (Read <-> Ready) -> Ended
                         |              |
                         +--> Closed <--+
```

1. **Open**: the client sends `replay`, `checkpoint`, `state.atomic`, or
   `output`. The open request has no payload. The server returns one JSON
   result (size/cursor for blobs, initial cursor for output) and no payload.
   The `state.atomic` result also includes `format`, which must equal the
   advertised `AtomicStateFormat` (`ghostty-vt-snapshot-v1`) before the payload
   is installed.
2. **Ready**: the client sends `*.read` with `maxBytes` in `1..MaxChunkBytes`.
   The server returns JSON metadata and at most `maxBytes` raw bytes. A zero
   byte, non-EOF read is invalid progress and is surfaced as `io.ErrNoProgress`.
3. **Ended**: the final read sets `eof:true`; its payload may be non-empty. The
   server closes the stream after that response. Clients return the bytes first
   and `io.EOF` on the next read (or immediately when the final payload is
   empty).
4. **Closed**: the client may send `*.close` from Ready. The server returns an
   empty JSON response and closes the connection. Closing locally is always
   allowed and unblocks a pending read.

Read, close, or open methods received in an inapplicable state are errors. A
stream never accepts arbitrary request payloads on control messages. A client
that cancels its context closes the Unix connection; the server observes the
disconnect and cancels any blocked process/output operation. Cancellation is
not converted to EOF or a successful empty result.

## Errors and boundaries

Errors are `{code,message}` with bounded messages. Stable sentinel mappings
include `protocol_mismatch`, `frame_too_large`, `invalid_cursor`,
`cursor_expired`, `session_not_found`, `session_closed`, `invalid_signal`, and
`process_done`. Unknown codes remain ordinary errors so newer servers can add
diagnostics without making older clients unsafe.

The peer that detects malformed JSON, an unsupported wire version, an invalid
length, a mismatched ID, a short payload, or an illegal payload/state transition
MUST fail the current connection or return a protocol error as appropriate. It
MUST NOT reinterpret malformed bytes as a valid subsequent frame.

## Compatibility and extension

Adding optional JSON members, capability names, methods, or result members is
forward-compatible only when an older peer can safely ignore them. The
`state.atomic` method is an additive capability; clients must check
`atomic-state-v1` before requiring it and must reject an unknown `format`.
Changing field meaning, framing, byte order, length semantics, stream ordering,
or required capabilities requires a new wire version or semantic protocol
version. No method may smuggle a new binary encoding behind an existing
`payloadBytes` field.

The `state.atomic` payload uses the bundled VT snapshot record stream. Its
`READY` marker follows the renderable state prefix and its `FINISH` marker
terminates the history records. Consumers may use the VT decoder's incremental
READY/next operations when they need to install the visible state before
loading older scrollback; the output cursor remains paired with the complete
state capture and must not be consumed until the renderable state is installed.
