# ghostline v1.0 release checklist

This checklist is the release gate for the first stable ghostline API. v1.0 is
intentionally incompatible with all v0 source APIs, cursor formats, and wire
methods. Do not add migration aliases to make this checklist pass.

## Supported platform statement

| Target | v1.0 claim | Evidence |
| --- | --- | --- |
| macOS 13+ amd64/arm64 | Full PTY, CGo VT, daemon, race, and fuzz support | Universal static archive; CI fixes and verifies deployment target 13.0 and runs native CGo jobs on Intel and Apple Silicon |
| Linux glibc 2.31+ amd64/arm64 | Full PTY, CGo VT, daemon, and race support | Per-architecture static archives and native CGo CI jobs; each test binary also runs in an Ubuntu 20.04 (glibc 2.31) container |
| FreeBSD amd64 | `CGO_ENABLED=0` cross-build only | CI `nocgo` job; session creation returns `ErrUnavailable` |
| Windows and other targets | Not supported by v1.0 | The Unix PTY/socket/FD-transfer/process-group runtime has no matching implementation |

The daemon is same-host and Unix-socket-only. Sessions do not survive daemon
crashes that lose PTY ownership, host reboot, or cross-machine migration.

## Candidate review

- [x] `ProtocolVersion` is exactly `1.0.0` and the release tag will be
      `v1.0.0`.
- [x] `go doc -all .` contains only the intended frozen v1 surface.
- [x] README, package docs, all RFCs, and the v1 migration note agree on API,
      ownership, durability, cancellation, and platform limits.
- [x] No v0 API or wire compatibility shim remains. Mentions of v0 are limited
      to rejection tests and the explicit incompatibility documentation.
- [x] The minimux example uses only public v1 APIs.
- [x] `git diff --check` is clean and generated release artifacts are absent
      from the worktree.

## Required local gates

Run from the repository root:

```sh
test -z "$(gofmt -l .)"
shasum -a 256 -c third_party/SHA256SUMS
go vet ./...
go test ./...
go test -race ./...
go test -run '^$' -bench . -benchtime=1x ./...
go test -run '^$' -fuzz '^FuzzParseCursorRoundTrip$' -fuzztime 3s
go test -run '^$' -fuzz '^FuzzRPCResponseDecoding$' -fuzztime 3s
go test -run '^$' -fuzz '^FuzzQueryResponderNeverPanics$' -fuzztime 3s
go test -run '^$' -fuzz '^FuzzVTStateRestore$' -fuzztime 3s
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./...
CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build ./...
```

The macOS release build must set `MACOSX_DEPLOYMENT_TARGET=13.0`; CI builds a
test executable and checks its `LC_BUILD_VERSION` with `vtool`. The bundled
archive is universal and each slice was built for macOS 13.0.

Run in the independent example module:

```sh
cd examples/minimux
go test ./...
go build ./...
go test -race ./...
```

Local result on 2026-08-25: every command above passed on macOS arm64,
including all four fuzz smoke targets and the saved malformed VT-state
regression. macOS amd64, Linux amd64, and Linux arm64 CGo test binaries also
cross-compiled successfully. The Linux arm64 binary ran the complete suite in
a Debian 12 arm64 container. A fresh macOS arm64 build with deployment target
13.0 produced `LC_BUILD_VERSION minos 13.0`.

Xcode 26's linker emits a non-fatal malformed `LC_DYSYMTAB` warning for the
Zig-built Ghostty Mach-O object during race linking. The warning reproduces
with a fresh Go cache and with both stripped and unstripped archives; `strip
-x` does not repair it. The resulting binaries link, report the correct
deployment target, and pass the full suite. Keep this visible until the pinned
Ghostty/Zig toolchain produces a canonicalized symbol table.

## Release procedure

- [ ] Push the candidate commit and require all GitHub Actions jobs and matrix
      entries to pass.
- [ ] Confirm both macOS architectures and both Linux architectures exercised
      CGo rather than the `ErrUnavailable` fallback.
- [ ] Review release notes for the explicit v0 incompatibility and the exact
      platform statement above.
- [ ] Create and push annotated tag `v1.0.0` only after every gate is green.
- [ ] Verify a fresh consumer can resolve `github.com/abcdlsj/ghostline@v1.0.0`,
      compile the README local and daemon examples, and observe a non-empty
      `TagVersion()` in the tagged build.
