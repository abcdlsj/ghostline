# libghostty-vt artifacts

ghostline statically links prebuilt `libghostty-vt` archives. Static linking
keeps the Go binary relocatable and avoids a runtime dependency on a repository
path, `.dylib`, or `.so` file.

## Source and toolchain

- Ghostty commit: `32192762fbbc9fa58a14407fb566fa5ad1f15ace`
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
| `third_party/lib/libghostty-vt.a` | macOS 13+, universal x86_64/arm64 | `caf85ee55ae5f964ad3ea02b26e7c71eafd1ab5888a68ac598cd8ae4fbf93e45` |
| `third_party/lib/linux_amd64/libghostty-vt.a` | Linux x86_64, glibc 2.31+ | `78390354a0bf85038158e9e6f56e973384a573266a440e03f1c9927f9f028638` |
| `third_party/lib/linux_arm64/libghostty-vt.a` | Linux aarch64, glibc 2.31+ | `4684dae520983fe406364c5d8598927e226949d7ac244bc8db342df95f04152e` |

Run `shasum -a 256 -c third_party/SHA256SUMS` from the ghostline repository
root to verify all three files. CI performs the same check before tests.
