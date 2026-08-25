# Binary migration rehearsal

The repository contains an opt-in process-level rehearsal for the irreversible
migration boundary. It uses real daemon executables rather than an in-process
test server.

Build the final v0 compatibility daemon from the compatibility branch and the
v1 daemon from the v1 branch:

```sh
git worktree list
go build -o /tmp/ghostline-v0.8 ./cmd/ghostline  # run in the v0.8 checkout
go build -o /tmp/ghostline-v1 ./cmd/ghostline     # run in the v1 checkout
```

Run the real binary handoff and crash-window matrix from the v1 checkout:

```sh
GHOSTLINE_V0_BINARY=/tmp/ghostline-v0.8 \
GHOSTLINE_V1_BINARY=/tmp/ghostline-v1 \
go test -count=1 -timeout=3m -run 'TestBinary(V0ToV1Handoff|MigrationCrashWindows)$' -v ./...
```

The handoff test creates a live session with the v0 binary, starts v1 with
`--adopt-from`, verifies the old output, writes new input through v1, and
checks that the v0 daemon retires.

The crash-window test kills a real source daemon immediately after each admin
phase:

- `list`: target must fail before serving;
- `adopt`: target must fail before serving;
- `snapshot`: target must fail before serving;
- `commit`: target must serve the adopted session even though source retirement
  is interrupted.

The v0 handoff intentionally rebuilds the v1 VT model and output generation
from v0 spool archives and the live spool. It does not attempt to decode the v0
native snapshot envelope. Ordinary unit and CI tests skip these process tests
unless the two binary environment variables are supplied.
