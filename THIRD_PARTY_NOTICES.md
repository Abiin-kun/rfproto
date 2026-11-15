# Third-party notices

## Scope of the MIT license

[LICENSE](LICENSE) applies to original rfproto source code and public
project documentation. It does not relicense the Inter font, Go dependencies,
native libraries, or any other third-party material. Those retain their own
copyrights and licenses. This file is a dependency/licensing guide, not a
complete license bundle or a certification that a particular binary is
redistributable.

## Bundled Inter font

- Files: `web/InterVariable.ttf` and `web/InterVariable.woff2`.
- Copyright (c) 2016 The Inter Project Authors (https://github.com/rsms/inter).
- License: SIL Open Font License 1.1; full text and copyright notice are in
  [web/Inter-LICENSE.txt](web/Inter-LICENSE.txt).

The native UI embeds the TTF in the executable. Distributing that executable
therefore distributes the font even when no separate font file is shipped.
Retain the font's copyright and OFL text with every distribution containing
it, including binary release packages. A standalone, accessible copy of
`web/Inter-LICENSE.txt` in the package is suitable. Font modifications remain
subject to the OFL, including any applicable naming restrictions; the font
must not be relicensed as MIT.

## External native libraries

The normal CGO build links dynamically to system libraries selected through
`pkg-config`. They are not covered by rfproto's MIT license. Native libraries
can themselves depend on further libraries with additional terms.

| Dependency | License considerations |
| --- | --- |
| SDL3 (`sdl3`) | zlib license. Preserve its notice in source redistributions and identify altered source versions as required by that license. |
| FFmpeg (`libavcodec`, `libavutil`, `libswscale`) | Normally LGPL-2.1-or-later, but optional components and configure flags can make the build GPL, with version requirements depending on the configuration. `--enable-gpl`, `--enable-version3`, external libraries, and `--enable-nonfree` matter; a nonfree configuration may not be redistributable. Inspect the exact build rather than assuming a distribution package is LGPL-only. See [FFmpeg's licensing guidance](https://ffmpeg.org/legal.html). |
| Opus (`opus`) | BSD-3-Clause. Retain the applicable copyright, conditions, and disclaimer in source and binary redistributions; review its accompanying patent notices as well. |
| FreeType (`freetype2`) | A choice of the FreeType License (FTL) or GPL-2.0-or-later for the main library, with some components carrying additional notices. Choose and comply with the applicable option for the exact package. Under the FTL, preserve the required notices and acknowledge use of FreeType in distribution documentation. |

rfproto uses the [FreeType Project](https://freetype.org) for text rendering;
this software is based in part on the work of the FreeType Team.

The SDL3, Opus, and FreeType descriptions above were checked against their
installed upstream license texts. They do not identify the license of an
uninspected release artifact. Package versions, patches, build flags, and
transitive dependencies must be checked again for each distribution.

## Go dependencies

Go modules listed in `go.mod` and `go.sum` retain their respective licenses;
rfproto's MIT license is not a replacement for them. Examples checked against
the listed versions' module license files are:

| Module | Version | License |
| --- | --- | --- |
| `github.com/godbus/dbus/v5` | `v5.2.4` | BSD-2-Clause |
| `github.com/gorilla/websocket` | `v1.5.3` | BSD-2-Clause |
| `github.com/grandcat/zeroconf` | `v1.0.0` | MIT |
| `github.com/pion/webrtc/v4` | `v4.1.6` | MIT |
| `github.com/skip2/go-qrcode` | `v0.0.0-20200617195104-da1b6568686e` | MIT |

This table is not an exhaustive inventory of the linked Go code. Include
indirect modules and the Go runtime/standard library when preparing binary
notices. Inspect the license and notice files in the exact module sources
used, including any separately licensed portions; do not infer all modules'
terms from a project family or from this table. Go code is normally compiled
into the executable, so its binary redistribution obligations apply even if
no module source directory is shipped.

## Before distributing binaries

A source license and permission to redistribute a linked executable are
separate questions. Dynamic linking does not eliminate license obligations
or make a GPL-enabled FFmpeg build equivalent to an LGPL build.

For each release:

1. Record the exact Go modules, toolchain, native library versions, build
   configurations, and any bundled/transitive libraries used by the artifact.
2. Review those exact sources' license, copyright, and notice files and the
   terms applicable to the resulting combination. Resolve incompatible or
   nonredistributable configurations before publishing.
3. Include rfproto's MIT license, the Inter OFL/copyright notice, and all
   required third-party license texts, attributions, and disclaimers in the
   release package. This summary alone does not replace those texts.
4. Fulfill applicable source, modification, relinking, and other LGPL/GPL
   requirements for the actual artifact and any libraries you redistribute.
   Account for system-library dependencies even when they are not bundled;
   document what recipients must install.

No license classification here is a patent clearance or legal certification.
