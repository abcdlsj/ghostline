# Refactor checklist

This document tracks the std-level refactor. Check an item only after it is
implemented and verified.

## Remote API parity

- [x] `Client.Start` sends `Size` and `Environment`
- [x] Remote `Session.Wait` returns the same `ExitError` as local
- [x] RPC preserves error identity so `errors.Is` works remotely
- [x] Every client method honors its `context.Context`
- [x] `Status` distinguishes a network error from a stopped session
- [x] Remote `CreatedAt` is fetched once and cached

## Session lifecycle

- [x] Remove the legacy `PTY` alias, `NewPTY`, and name-based compatibility API
- [x] `Session.Close` terminates and keeps the record; `Session.Remove` deletes it
- [x] `Start` rejects a name that already exists, live or stopped
- [x] Watchers belong to a session and close with it
- [x] Spool maintenance is part of `Session` (`Recover`, `TruncateSpool`,
  `ArchiveSpool`, `RemoveSpool`)
- [x] Drop `.created`/`.pid` metadata files; no orphan adoption semantics

## Server robustness and security

- [x] Unix socket is `chmod 0600`
- [x] Per-connection idle deadline and message size limit
- [x] Concurrent connection cap
- [x] `Serve` takes a `context.Context`
- [x] `Server.Shutdown` terminates sessions
- [x] `cmd/ghostline serve` handles termination signals

## Structure and naming

- [x] Replace `Manager` terminology with `Hub` everywhere
- [x] Reorganize files into focused units (`hub`, `session`, `process`,
  `rpc`, `client`, `server`, `terminal`, `spool`, `query`)
- [x] Rename tests to match the new file layout
- [x] Move `examples/minimux` into its own module; root depends only on
  `creack/pty`
- [x] Document Unix-only platform support
- [x] Keep one public API surface with no deprecated aliases

## Polish

- [x] Complete doc comments for every exported identifier
- [x] Make `SpoolWatcher.maxBytes` race-free
- [x] Release `QueryResponder.pending` capacity when dropped
- [x] Replace wall-clock `WaitReady` polling with context-aware `Check`
- [x] Pass a clean code-style pass: short-lived names, flat control flow,
  minimal comments

## Tests and CI

- [x] Remote/local parity tests (options, exit error, errors, checkpoint,
  recovery)
- [x] Watcher isolation test when a name is removed and reused
- [x] Socket permission test
- [x] Malformed and oversized request tests
- [x] Fuzz coverage for the RPC line parser and `QueryResponder`
- [x] CI workflow (macOS full tests, Linux no-cgo build, gofmt/vet)

## Beauty pass

- [x] `session.go`: drop the 19-method forwarding layer; embed small composed
  interfaces; make `Status` a public struct with exit information
- [x] `hub.go`: keep the name reservation lock short; start the child outside
  the lock with a pending reservation
- [x] `process.go`: make termination bounded and event-driven; stop overriding
  `TERM`/`COLORTERM`/`NO_COLOR` for the child
- [x] `client.go`/`server.go`: replace remote wait polling with a blocking
  `wait` RPC and cancellation on disconnect
- [x] `server.go`: replace repeated inline params with shared param types and
  a small decode helper
- [x] `rpc.go`: add the decode helper and shared request types
- [x] `errors.go`/`size.go`/`spool.go`/`query.go`/`terminal.go`/`util.go`:
  remove noise comments, keep only why-comments
- [x] Tests: update environment semantics, remote wait, and status assertions
- [x] README: document inherited environment and event-driven remote waits

## File review

- [x] `doc.go`
- [x] `errors.go`
- [x] `size.go`
- [x] `hub.go`
- [x] `session.go`
- [x] `process.go`
- [x] `spool.go`
- [x] `query.go`
- [x] `terminal.go` / `terminal_nocgo.go`
- [x] `rpc.go`
- [x] `client.go`
- [x] `server.go`
- [x] `util.go`
- [x] `cmd/ghostline/main.go`
- [x] `examples/minimux/main.go`
- [x] tests
