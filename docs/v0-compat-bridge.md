# v0.x compatibility bridge

The final v0.x release is `0.8.0`. It keeps the v0 public API and JSON-lines
RPC unchanged for existing clients, while publishing an explicit handoff
contract for a later v1 daemon.

## Two-stage upgrade

Warren may perform the upgrade in two idempotent stages:

1. Start the v0.8 daemon against the existing v0 socket. Its normal rolling
   adoption path first absorbs older v0.4/v0.5 spool sources when required.
2. On a later restart, start the v1 daemon with the v0.8 admin socket as its
   handoff source. The v1 coordinator must require
   `handoffVersion == "ghostline-v0-to-v1-1"` before using the compatibility
   fields, then switch clients only after the all-or-nothing commit succeeds.

ghostline does not silently replace a public v0 socket with a v1 protocol. The
coordinator owns discovery, process startup, socket switching, and retirement;
the bridge only exposes deterministic source metadata and migration semantics.

## Handoff metadata

The v0.8 admin `list` response adds optional fields that older v0 migration
clients ignore:

- `version`: `0.8.0`, the source daemon's public protocol version;
- `handoffVersion`: `ghostline-v0-to-v1-1`;
- `spoolPath`: the shared raw spool path;
- `spoolSize`: the source's current raw spool size;
- `spoolFormat`: `ghostline-v0-spool-1`.

The existing `list`, `adopt`, `snapshot`, `commit`, `abort`, and `exit` admin
sequence remains all-or-nothing. Live PTY masters still transfer with
`SCM_RIGHTS`; the v1 destination reconstructs its own output log from the
shared v0 spool and archived segments instead of treating v0 byte offsets as
v1 cursors.

## Safety boundaries

- The v0 public RPC remains available until the coordinator switches clients.
- A v1 destination must reject unknown or missing handoff versions before
  preparing a session.
- A failed preparation aborts the complete batch and leaves the v0 daemon as
  owner.
- The bridge is same-host and requires the output directory to be shared.
- It is not a v0/v1 wire compatibility shim for arbitrary mixed requests, and
  it does not make daemon crashes, reboot, or cross-host migration durable.
