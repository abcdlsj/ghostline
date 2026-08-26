# libghostty-vt artifacts

ghostline statically links prebuilt `libghostty-vt` archives. Static linking
keeps the Go binary relocatable and avoids a runtime dependency on a repository
path, `.dylib`, or `.so` file.

## Source and toolchain

- Ghostty commit: `88f57ee66eeaad4da77b414b245f7b6693348985`
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
| `third_party/lib/libghostty-vt.a` | macOS 13+, universal x86_64/arm64 | `53c7466cf10784db9aeb40623bc66bdb9211e883447a6e55aa8eb508617ec75e` |
| `third_party/lib/linux_amd64/libghostty-vt.a` | Linux x86_64, glibc 2.31+ | `ead758370bb255db90ee00a009531ba02b936bafc8d5be29e5c4c806ec8219bc` |
| `third_party/lib/linux_arm64/libghostty-vt.a` | Linux aarch64, glibc 2.31+ | `e4383a86372d6376b9b08cb7a37e2fa12e61dab39804904fb6090d16d00f318e` |

Run `shasum -a 256 -c third_party/SHA256SUMS` from the ghostline repository
root to verify all three files. CI performs the same check before tests.
