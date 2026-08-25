# ghostline v1 implementation plan

Status: v1 implementation complete; awaiting remote platform matrix and release tag

Current checkpoint (2026-08-25): all planned v1 mechanisms are implemented in
the working tree. Platform packaging now uses a macOS universal static archive
with a 13.0 deployment target and Linux glibc 2.31 static archives for amd64
and arm64, all built from pinned Ghostty commit
`5851d98615187d85052e41042bcf66e0ccec11d4` with Zig 0.16.0. The old macOS
dylibs and absolute source-tree runtime path are removed. Local macOS arm64
tests and race tests pass; a fresh deployment-target build reports
`LC_BUILD_VERSION minos 13.0`, and the macOS amd64 CGo test binary
cross-compiles with the same minimum. Both Linux CGo test binaries
cross-compile, and the Linux arm64 binary passes the complete suite natively
in a Debian 12 arm64 container. CI now has native macOS Intel/Apple Silicon and
Linux amd64/arm64 matrices, artifact checksum checks, and a final macOS binary
deployment-target check. Remote CI and a native Linux amd64 run remain pending,
so the two platform completion items stay open.

Delegation note: attempts to run the explicitly requested `codex-luna-max`
sessions through Warren were blocked before model startup by Codex's repository
trust prompt. Warren reported no independent agent thread or transcript, and
the current Warren version rejects Codex under generic `session create`.
Zombie sessions were removed and no delegated changes were accepted. A second
attempt on 2026-08-24 attached the raw trust TTY and sent both Enter and `1`,
but Warren only repainted the prompt and Codex still created no transcript;
that agent was also removed without changes. Retry delegation only after
repository trust is persisted or Warren can drive that first-run prompt;
never treat the fallback parent transcript as agent output.

A third attempt on 2026-08-25 used the current `warren agent create` flow with
the requested `codex-luna-max` command. Warren created agent
`d82e304b-abc2-472c-933b-4d28fcf4da5f`, but `agent read` confirmed that it was
still waiting for repository trust and had no agent thread. Live attach plus
Enter only repainted the prompt again. The zombie was removed and made no
changes.

This document is the durable source of truth for the v1 rewrite. Read it before
continuing work after a context reset. v1 is intentionally incompatible with
all v0 APIs and protocols. Do not add compatibility shims or legacy migration
bridges.

## Product boundary

ghostline v1 is a local-first terminal session runtime for Go. It owns PTYs,
keeps sessions independent from clients, exposes resumable raw output, and
creates atomic checkpoints that bind a VT replay to an output cursor.

The core is not a terminal UI, multiplexer, cross-host remote-shell protocol,
or reboot-persistent process manager. A daemon package may provide same-host
process separation and rolling upgrades without leaking those policies into
the core session contract.

## Non-negotiable v1 rules

- No v0 source or wire compatibility.
- No hidden I/O in methods that lack `context.Context` and an error result.
- No network or storage failure may be converted into an empty result, false,
  zero time, or discarded error.
- Public session handles are concrete. Consumers define their own narrow
  interfaces.
- `Close` releases a handle or stream. Process termination and record deletion
  use explicit verbs.
- Shell evaluation is explicit. The primary process specification is argv.
- Output positions include a generation and offset. A bare file offset is not
  a durable cursor.
- Output recovery is streamed and bounded. Public APIs do not allocate an
  arbitrary requested range into one `[]byte`.
- The core owns output mechanics. Compression, archive count, and retention
  decisions are caller policy.
- The supported platform claim must match the tested CGo matrix.

## Work plan

### 1. Core public contract

- Replace the provider-defined `Session` interface with a concrete `Session`.
- Give `Hub` and daemon `Client` matching `Start`, `Get`, and `List` signatures.
- Keep only cached immutable accessors context-free.
- Replace `Alive` with `Status(ctx)`.
- Replace `Input` with `WriteInput(ctx, data)`.
- Rename screen output to `Replay(ctx)`; reserve snapshot terminology for
  emulator state encoding.
- Replace session `Close` and `Remove` with `Terminate(ctx)` and `Delete(ctx)`.
- Integrate metadata into the concrete handle instead of a capability type
  assertion.
- Introduce an argv-first process specification and an explicit shell-command
  helper or field.

### 2. Output log and cursor

- Introduce an opaque/comparable cursor containing generation and offset.
- Replace in-place truncation with immutable completed segments plus one active
  segment.
- Make rotation change generation before the new active segment is published.
- Ensure readers drain completed generations in order and cannot miss a fast
  truncate-and-regrow transition.
- Make checkpoints atomically pair replay bytes with the current cursor.
- Expose a bounded `OutputReader` (`io.ReadCloser` plus current cursor), with
  natural backpressure and cancellation.
- Expose mechanism-only rotation/pruning operations if embedders need them;
  remove gzip and fixed archive-count policy from core sessions.

### 3. Daemon and RPC

- Move daemon lifecycle and rolling-upgrade concerns behind a daemon-facing
  package boundary or an equally strict public boundary with no core leakage.
- Stream output through the protocol. Do not require clients to open server
  spool paths.
- Bound request and response frames and chunk all large payloads.
- Remove context-free RPC, swallowed errors, hidden retries, and uncancellable
  `Done` polling goroutines.
- Split plain dialing from managed server bootstrap; managed behavior must be
  explicit in its constructor/name.
- Bump the protocol directly to v1 and delete all pre-v1 compatibility bridges.
- Preserve all-or-nothing same-version PTY and VT-state adoption.

### 4. VT and policy boundaries

- Keep Ghostty-specific VT implementation out of the primary session API.
- Return errors from operations that can reject input instead of silently
  ignoring failures.
- Remove package-global logging; use returned errors or explicit optional
  hooks.
- Move process probing and storage retention policy to explicit options or
  focused packages.
- Use bounded diagnostic buffers for managed daemon startup.

### 5. Naming and documentation

- Rewrite package docs and README around “local-first terminal session
  runtime”.
- State exact durability limits: client detach and daemon upgrade survive;
  daemon crash, host reboot, and cross-machine migration do not.
- Document ownership, cancellation, backpressure, cursor validity, checkpoint
  ordering, and callback/goroutine behavior.
- Update the CLI and minimux example to use only v1 APIs.
- Add a v1 architecture RFC and migration note that plainly states v0 is not
  compatible.

### 6. Quality gates

- Unit tests for every public method and sentinel error.
- Local/daemon semantic conformance tests using the same test suite.
- Segment rotation and generation-race tests, including rapid regrowth.
- Stream cancellation, backpressure, frame-limit, and large-output tests.
- Same-version rolling-upgrade tests with live PTYs and VT state.
- Fuzz query parsing, RPC decoding, cursor decoding, and snapshot restoration.
- Benchmarks for output throughput, checkpoint latency/memory, session fanout,
  and rotation.
- Run `gofmt`, `go vet ./...`, `go test ./...`, `go test -race ./...`, example
  tests, `CGO_ENABLED=0` builds, and supported-platform compile checks.
- Expand CI so every advertised platform executes the strongest feasible CGo
  test job. Do not advertise an untested platform as fully supported.

### 7. Platform expansion

- [x] Build a universal macOS libghostty-vt with a macOS 13.0 deployment
      target.
- [x] Configure native macOS Intel/Apple Silicon CGo and race CI, with a fresh
      target-13 build and final binary metadata assertion.
- [x] Package reproducible Linux amd64 and arm64 libghostty-vt artifacts from
      the pinned Ghostty commit and configure native CGo CI for both.
- [x] Run the complete Linux arm64 suite natively in a Debian 12 arm64
container; retain the native Linux amd64 run as a remote CI gate.
- [x] Investigate the Xcode 26 `LC_DYSYMTAB` race-link warning with a fresh Go
      cache and stripped, unstripped, and `strip -x` archives. It is emitted
      for the Zig-built Ghostty Mach-O object in all variants; binaries link
      and pass, so retain the reproducible warning in the release checklist.
- [x] Keep FreeBSD at no-CGo compile-only status until a native CGo job exists.
- [x] Keep Windows outside v1 because the runtime requires Unix PTYs, Unix
      sockets, file-descriptor transfer, and Unix process-group signals.
- [x] Document source commit, Zig version, build commands, minimum OS versions,
      artifact architectures, checksums, license, and exact support claims.
- [ ] Observe every remote platform matrix entry passing on the candidate
      commit.

### 8. Performance qualification and control-plane freeze

- [x] Replace one-iteration benchmark smoke with bounded, repeatable
      benchmarks for output append/read throughput, local versus daemon RPC,
      checkpoint state sizes, session fanout, and control-plane operations.
- [x] Run multi-sample benchmarks with allocation reporting, record the v1
      hardware/toolchain baseline, and define a same-host `benchstat` release
      comparison policy. Do not use noisy hosted-runner timing as a hard test.
- [x] Add a CI performance job that preserves benchmark results as artifacts
      and catches hangs, unbounded storage growth, and gross regressions.
- [x] Audit the exported control plane for orthogonal mechanisms required after
      client reattachment. Candidate additions are process-group Signal,
      current PTY Size, and the current raw-output Cursor; reject rename,
      policy-level suspend/resume, retention policy, and external PID escape
      hatches unless evidence requires them.
- [x] Implement accepted control operations for local and daemon sessions with
      shared conformance, cancellation, validation, stopped-session, and RPC
      coverage.
- [x] Re-run the complete release gate and review `go doc -all .` as the final
      v1 surface.

### 9. Extensible wire foundation

- [x] Separate the wire framing version from the semantic RPC protocol and VT
      snapshot versions. Reject unsupported framing with an inspectable
      sentinel error.
- [x] Give every request and response a bounded JSON envelope plus an optional
      exact-length raw payload. Reuse the framing for input, output, Replay,
      and Checkpoint instead of method-specific base64 encodings.
- [x] Publish capabilities and negotiated limits through VersionInfo; allow
      unknown JSON fields and capability names while keeping required fields
      strict.
- [x] Specify connection sequencing, IDs, stream open/read/close state,
      cancellation, EOF, short payloads, maximum sizes, error codes, and
      compatibility rules in a dedicated wire RFC.
- [x] Add malformed envelope/payload, unsupported version, length boundary,
      stream-state, and fuzz coverage before freezing wire v1.

## Completion checklist

- [x] Core public contract implemented.
- [x] Generational segmented output implemented.
- [x] Streaming local and daemon output implemented.
- [x] v0 protocol and API compatibility code removed.
- [x] Daemon/VT/policy boundaries implemented.
- [x] CLI, example, README, package docs, and RFC updated.
- [x] Unit, conformance, integration, fuzz, benchmark, race, and platform tests
      added and passing.
- [x] Exported API reviewed as a v1 freeze candidate.
- [x] Release checklist and remaining platform limitations documented.
- [ ] macOS 13+ universal CGo runtime and CI verified.
- [ ] Linux amd64/arm64 CGo runtime and CI verified.
- [x] Performance qualification baseline and CI artifact job complete.
- [x] Final control-plane additions implemented and API freeze reviewed.
- [x] Wire framing v1 implemented, documented, fuzzed, and frozen.

## Continuation protocol

After a context reset:

1. Read this file completely.
2. Inspect `git status --short` and preserve unrelated changes.
3. Read the latest test output or rerun the narrowest relevant test.
4. Update the status and checklist in this file whenever a phase is completed.
5. Continue the first incomplete phase; do not restart completed work.
