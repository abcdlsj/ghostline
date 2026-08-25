# v0.x compatibility handoff

The v0.8 daemon is the final compatibility source for a v1 daemon. It keeps
the v0 public RPC unchanged and exposes the opt-in admin handoff
`ghostline-v0-to-v1-1`.

Warren owns discovery, process startup, socket switching, and source
retirement. The ghostline v1 consumer accepts only source protocol `0.8.0`
with the exact handoff identifier. Unknown or missing handoff versions are
rejected before any session is prepared.

For each live session, v1 transfers the PTY master and replays the v0 archived
and live spool files into both a fresh v1 VT model and a new v1 output log.
The v0 native snapshot envelope is deliberately not decoded by v1. The
destination starts at generation one and creates fresh opaque cursors; v0 byte
offsets and spool sizes are never interpreted as v1 cursors.

The handoff is same-host and all-or-nothing. A preparation failure aborts the
complete batch and leaves v0 as owner. The public v1 socket remains strictly
v1 and never negotiates or serves v0 requests.
