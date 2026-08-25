# v1 performance baseline

Performance qualification verifies that the stdlib-grade implementation has
no unbounded storage, protocol amplification, or obvious allocation
regression. It does not present shared CI runner variance as a stable latency
promise.

## Reproducible environment

Local baseline (2026-08-25):

- Apple Mac15,3, Apple M3, arm64; Darwin 25.4.0
- Go `go1.25.1 darwin/arm64`
- Ghostty VT commit `5851d98615187d85052e41042bcf66e0ccec11d4`
- Zig `0.16.0`

Command:

```sh
go test -run '^$' -bench . -benchmem -benchtime=500ms -count=10 ./...
```

The completed v1 baseline took 289 seconds. It used
`benchstat v0.0.0-20260819171926-ebcb4798430d`, installed temporarily with Go
1.26.7. Release automation must retain its raw output with the build record.

The suite covers output append and rotation/pruning, retained-output reads for
local and daemon transports, 4 KiB/256 KiB/1 MiB checkpoints, eight-reader
fanout, Status/Size/OutputCursor/Signal, listing 100 sessions, and Cursor
parsing. Output logs rotate and prune periodically, so a longer `-benchtime`
cannot grow disk use without bound.

## Observed baseline

The following are benchstat central values from the command above. They are
single-host measurements, not cross-platform promises.

| Operation | Time | Throughput | Heap allocation |
| --- | ---: | ---: | ---: |
| Append 64 KiB output | 55.83 us | 1.093 GiB/s | 112 B, 1 alloc/op |
| Rotate and prune | 76.08 us | — | 791.5 B, 8 allocs/op |
| Read 4 MiB retained output, local | 140.0 us | 27.91 GiB/s | 488 B, 6 allocs/op |
| Read 4 MiB retained output, daemon | 44.77 ms | 89.42 MiB/s | 4.241 MiB, 3,056 allocs/op |
| Checkpoint, daemon, 4 KiB history | 414.8 us | — | 42.74 KiB, 116 allocs/op |
| Checkpoint, daemon, 256 KiB history | 963.0 us | — | 1.021 MiB, 211.5 allocs/op |
| Checkpoint, daemon, 1 MiB history | 910.4 us | — | 981.2 KiB, 212 allocs/op |
| Eight-reader 1 MiB fanout, local | 292.3 us | 26.72 GiB/s aggregate | 516.6 KiB, 67 allocs/op |
| Eight-reader 1 MiB fanout, daemon | 50.69 ms | 158.0 MiB/s aggregate | 9.497 MiB, 6,891 allocs/op |
| Daemon control-plane call | 34.48–53.61 us | — | 19.97–20.35 KiB, 61–74 allocs/op |
| List 100 daemon sessions | 203.5 us | — | 99.80 KiB, 679 allocs/op |

The daemon output and fanout measurements varied by 25–33% on this host, so
they are treated as throughput ranges rather than latency SLOs. The transport
is suitable for terminal traffic and bounded output recovery; it is not
positioned as a bulk-data transport. The raw-payload framing removes base64
amplification, while the remaining per-chunk RPC work is visible in daemon
allocation counts.

## Comparison policy

Before release, save two raw outputs on the same host with the same
Go/Ghostty toolchain and command, then compare them with the same `benchstat`
version. Preserve both raw outputs and the benchstat version in the build
record. A statistically significant throughput decrease or allocation increase
greater than 15% is a release-blocking signal only after the affected benchmark
reproduces. Absolute ns/op values, scheduling variance, and cross-architecture
results from shared GitHub runners are not hard gates.

Suggested command (the tool is installed in a temporary directory and does
not modify project dependencies):

```sh
GOBIN=/tmp/ghostline-tools go install golang.org/x/perf/cmd/benchstat@latest
/tmp/ghostline-tools/benchstat old.txt new.txt
```

## CI

The CI `performance` job uses fixed `-benchtime=200ms -count=3` runs to catch
hangs, timeouts, unbounded growth, and order-of-magnitude regressions. It
uploads a `ghostline-bench` artifact. It intentionally has no noisy hard
latency threshold on shared runners; release candidates still follow the
same-host, multi-sample policy above.
