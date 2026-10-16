# Licensing and attribution

## Original tcdpool work

Dave selected GNU General Public License version 3 or, at your option, any
later version (SPDX: `GPL-3.0-or-later`) for the original tcdpool work.
This applies to the original code in `cmd/` and `internal/`, original test
code in `tests/`, and original documentation, configuration and build scripts.
The full GNU GPL version 3 text is in `LICENSE`.

This program is free software: you can redistribute it and/or modify it
under the terms of the GNU General Public License as published by the Free
Software Foundation, either version 3 of the License, or (at your option)
any later version. This program is distributed in the hope that it will be
useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See `LICENSE`.

This grant covers only material the tcdpool contributors are entitled to
license. It does not replace third-party licenses, remove copyright or
attribution notices, or grant rights to proprietary firmware.

## Included third-party material

### tcd patches

`patches/tcd-nohw/` contains modifications to nostar/tcd and upstream context.
Retain upstream notices and distribute the patches under GPL-3.0-or-later.
The patch headers identify their authors, dates and changes. Their target is:

- https://github.com/nostar/tcd
- branch `tcd-nohw`, commit `e347df69c0775a1f3e16298e098bfba313186ab6`
- upstream `LICENSE` and source headers, including `Controller.cpp`

### IMBE vocoder

`third_party/imbe_vocoder/` comes from `src/imbe_vocoder` in the local urfd
commit `172850c8888d9c7b301ec80c0fa15e3722bb15e3`. All 56 included files match
that commit byte for byte. Its README identifies the library as originating
in osmocom OP25 and links https://github.com/nostar/imbe_vocoder.

The implementation headers attribute Pavel Yazev (2009) and specify
GPL-3.0-or-later. Existing copyright notices remain authoritative. Some
wrapper/build files have no individual license grant; this document does
not invent one. `third_party/imbe_vocoder/COPYING` supplies the GPLv3 text
referenced by the implementation headers; adding it does not relicense files.
The stated urfd commit was verified locally, not against a public upstream.

### md380 vocoder and firmware utility

`third_party/md380_vocoder_dynarmic/` comes from the same urfd commit's
`src/md380_vocoder_dynarmic`. All nine included files match that commit byte
for byte. The upstream SRAM image was deliberately not included.
The existing `LICENSE` is GPLv3. `md380_vocoder.h` attributes Doug McLain
AD8DP (2020-2021), credits MD380Tools, and specifies GPL-2.0-or-later.
Retain that grant; using its GPLv3 option is compatible with tcd.
Some other files have no individual license header.

**Custom licensing uncertainty retained:** `firmware/md380-fw.py` is derived from
MD380Tools and has no individual license header. MD380Tools uses a custom
beerware license, not the GPL and not a presumed attribution-only license.
The neighboring GPL text does not establish a replacement grant for it.

Authoritative references:

- https://github.com/travisgoodspeed/md380tools/issues/837
- licensing change `77865a0f87eba0900181422126c90fb83f5d4cf2`
- https://github.com/travisgoodspeed/md380tools/blob/d7f4206c146a7580da5ff55c71784be6fc06b282/README.md

The README describes licensing in exchange for two liters of India Pale
Ale delivered at a neighboring bar. The maintainer explicitly confirmed
unifying the Python scripts under the beerware license. Attribution alone
has not been established as sufficient. No claim is made here that its
condition has been satisfied or that permission has been obtained.

The utility exists from the repository's first commit (`3f7cb50`). Deleting
its current copy would not remove it from a full-history publication.
The repository owner elected to publish the source with its full history
after reviewing this uncertainty. That decision does not amend upstream
terms, establish that their condition has been satisfied, or represent that
permission was obtained. The utility and its history are preserved.

## Dependencies fetched by builders

These are not relicensed by the root license. Preserve the applicable
licenses and notices when redistributing their sources or compiled code.

| Component | Revision or evidence | License |
|---|---|---|
| coder/websocket | `v1.8.15`, `go.mod`; upstream `LICENSE.txt` | ISC |
| nostar/tcd | pinned commit above; `Controller.cpp`, `LICENSE` | GPL-3.0-or-later |
| Codec2 in tcd | pinned tcd `codec2/` source headers | LGPL-2.1; check each file's notice |
| Kiss FFT in tcd | pinned tcd `codec2/kiss_fft.cpp` | BSD-3-Clause |
| mbelib-derived code in tcd | pinned tcd `mbe/` notices | ISC |
| Dynarmic | `9d4582339990d4eae53f1dc7160686920fc2075c`, upstream `LICENSE.txt` | 0BSD |
| Dynarmic externals | corresponding pinned `externals/*/LICENSE*` | MIT for fmt, mcl, Zycore, Zydis, robin-map, oaknut and biscuit; BSL-1.0 for Catch |
| Boost | builder's installed package | BSL-1.0 |
| Go toolchain and Debian packages | builder's selected versions | Their own licenses; package-specific inventory required for redistribution |
| TYT firmware and SRAM core | firmware fetched by `download.sh`; core provided by builder | Redistribution permission not established; not supplied in this repository |

## Source-only release scope

The intended release is source only. No binaries, container images, TYT
firmware or SRAM core images are supplied. Users build for themselves and
must determine their rights to obtain and use the external firmware.
The firmware and core are compiled into the vocoder library during builds;
this document does not authorize distributing the resulting binaries/images.
No noncommercial-only or amateur-radio-only restriction is added to the
original code's GPL grant. Copyright licenses do not settle any applicable
third-party patent rights.
