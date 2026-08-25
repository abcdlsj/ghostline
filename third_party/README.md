# libghostty-vt artifacts

ghostline statically links prebuilt `libghostty-vt` archives. Static linking
keeps the Go binary relocatable and avoids a runtime dependency on a repository
path, `.dylib`, or `.so` file.

## Source and toolchain

- Ghostty commit: `5851d98615187d85052e41042bcf66e0ccec11d4`
- Zig: `0.16.0`
- Optimization: `ReleaseFast`
- License: MIT; see [LICENSE.ghostty](LICENSE.ghostty)

The following commands run from a clean checkout of that Ghostty commit. Each
`PREFIX` must name a fresh output directory.

### macOS 13 universal archive

```sh
zig build \
  -Demit-lib-vt \
  -Dtarget=x86_64-macos.13.0 \
  -Doptimize=ReleaseFast \
  -p "$PREFIX"
strip -S \
  "$PREFIX/lib/ghostty-vt.xcframework/macos-arm64_x86_64/libghostty-vt.a"
```

Copy the resulting archive to `third_party/lib/libghostty-vt.a`. It contains
both `x86_64` and `arm64` slices, each with a macOS 13.0 deployment target.

### Linux glibc 2.31 archives

```sh
zig build \
  -Demit-lib-vt \
  -Dtarget=x86_64-linux-gnu.2.31 \
  -Doptimize=ReleaseFast \
  -p "$PREFIX"

zig build \
  -Demit-lib-vt \
  -Dtarget=aarch64-linux-gnu.2.31 \
  -Doptimize=ReleaseFast \
  -p "$PREFIX"
```

Copy `$PREFIX/lib/libghostty-vt.a` from the x86_64 build to
`third_party/lib/linux_amd64/libghostty-vt.a`, and copy the same output path
from the aarch64 build to `third_party/lib/linux_arm64/libghostty-vt.a`.

## Artifact manifest

| Path | Target | SHA-256 |
| --- | --- | --- |
| `third_party/lib/libghostty-vt.a` | macOS 13+, universal x86_64/arm64 | `4189d3e79377fb01c46582e961f010fe482e4680b1a437dab150901d3788a7cb` |
| `third_party/lib/linux_amd64/libghostty-vt.a` | Linux x86_64, glibc 2.31+ | `095617d19af2f16c3c0ab5e14cdfe428fb74f8f094a693f7660e405e122a1b85` |
| `third_party/lib/linux_arm64/libghostty-vt.a` | Linux aarch64, glibc 2.31+ | `426e8aa84d761a632b62a77f4125ec7f478f835e21422b6795076638a9cca0e0` |

Run `shasum -a 256 -c third_party/SHA256SUMS` from the ghostline repository
root to verify all three files. CI performs the same check before tests.
