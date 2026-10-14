# The tcd link

This document describes the TCP link between tcdpool and one tcd
process (nostar tcd, branch tcd-nohw, with the patches in
`patches/tcd-nohw`). The same link connects tcd to a urfd reflector.
tcdpool takes the place of the reflector.

The Go code is `internal/backend/nostar`. The C++ code is in the
patches (`TCPacketDef.h`, `TCPacketCodec.*`, `TCFraming.*`,
`TCSocket.cpp`, `Controller.cpp`).

## 1. Connection

1. tcdpool listens on a loopback port (`port_min` to `port_max`, in
   1025..49000, a tcd limit).
2. tcdpool writes a `tcd.ini` with this port, one module letter
   (A to Z) and the gains, and starts tcd with the ini file.
3. tcd connects to the port and sends one byte: its module letter.
   This byte has no frame.
4. After that byte, both sides send packets (section 2), in raw or v2
   framing (sections 3 and 4). The framing is set in `tcd.ini`
   (`Framing = raw` or `Framing = v2`). Both sides must use the same
   framing.
5. tcd sends a ping packet about each 5 s. tcdpool reads the link as
   stalled when it gets nothing for 20 s (`watchdog`).

The patched tcd sets `TCP_NODELAY` on the link. Without it, Nagle's
algorithm holds a reply until the ACK of the reply before it, and the
peer can delay that ACK until it sends the next frame. With frames at
20 ms this adds about one frame to each reply (unpatched: 21 ms
average, patched: 2.9 ms; `wiretest` scenario `raw_paced_latency`).
tcdpool also sets `TCP_QUICKACK` before each read, for an unpatched
tcd.

## 2. The packet (STCPacket)

The packet is 376 bytes. Upstream tcd sends the memory of the C++
struct `STCPacket`. The patched tcd writes and reads each field
explicitly, with the same bytes, so the two can talk.

Measured with g++ 14.2 on aarch64 Linux: size 376, alignment 4. The
`static_assert` lines of patch 0001 also hold for x86-64, i386, arm64
and armv7 (checked with clang).

| Offset | Size | Field | Content |
|---|---|---|---|
| 0 | 4 | sequence | u32 LE. |
| 4 | 1 | module | The module letter, `A` to `Z`. |
| 5 | 1 | is_last | `0` or `1`. |
| 6 | 2 | streamid | u16 LE. |
| 8 | 1 | codec_in | ECodecType (table below). |
| 9 | 9 | dstar | D-Star AMBE. |
| 18 | 9 | dmr | AMBE+2. |
| 27 | 16 | m17 | Two Codec2 3200 frames (or one Codec2 1600 frame). |
| 43 | 11 | p25 | IMBE. |
| 54 | 320 | usrp | 160 PCM samples, s16 LE. |
| 374 | 2 | padding | `0`. |

ECodecType:

| Value | Name | Input field |
|---|---|---|
| 0 | none | - |
| 1 | dstar | dstar |
| 2 | dmr | dmr |
| 3 | c2_1600 | m17 |
| 4 | c2_3200 | m17 |
| 5 | p25 | p25 |
| 6 | usrp | usrp |
| 255 | ping | - (the patched tcd sets all fields to 0, except module) |

Codec2 3200: tcd puts two 20 ms frames in one m17 field. A packet with
an even sequence uses bytes 0..7 of m17, an odd sequence bytes 8..15.
tcdpool maps the 8-byte `codec2_3200` payload of the client to and from
the correct half.

The patched tcd refuses a packet with an unknown `codec_in`, an
`is_last` that is not 0 or 1, or a module letter that is not its own.
With raw framing it then closes the link. With v2 framing it drops the
frame and continues (section 4).

## 3. Raw framing

Each packet is 376 bytes on the stream, with nothing between packets.
This is the upstream format.

One lost or extra byte puts all the next packets out of step. The
reader cannot find the start of the next packet. tcdpool reads each
packet with a full read (no partial packets), checks it, and restarts
the tcd process on bad data (counted as `desyncs`).

With raw framing, tcd fills the codecs in `DefaultOutputs` of
`tcd.ini` (default: all).

## 4. v2 framing

v2 is an opt-in change of the patches. Upstream tcd does not have it.
tcdpool uses v2 by default (`backend.framing`).

Each packet, pings too, is one frame of 388 bytes:

| Offset | Size | Field | Content |
|---|---|---|---|
| 0 | 4 | magic | `TCD2` (`54 43 44 32`). |
| 4 | 1 | version | `1`. |
| 5 | 1 | outputs | Requested-outputs mask (section 5). |
| 6 | 2 | length | Payload length, u16 LE. `376` for version 1. A reader accepts 376..1024 and ignores the bytes after 376. |
| 8 | 376 | payload | The packet (section 2). |
| 384 | 4 | crc | CRC-32 (IEEE 802.3, as zlib `crc32`) of bytes 4..383, u32 LE. |

The reader:

- skips bytes until it finds the magic,
- refuses a frame with a bad version, length or CRC, or with a packet
  that is not correct (section 2), and continues at the next magic,
- drops the link after 8 bad frames with no good frame between them,
  or after 64 KiB of bytes that are not in a frame.

tcdpool counts bad frames (`link.bad_frames`) and skipped bytes
(`link.skip_bytes`). A frame with no reply is counted as missing. A
reply for a frame that tcdpool did not send is a desync: tcdpool then
restarts tcd.

## 5. The outputs mask

Upstream tcd decodes each packet and encodes it into every codec. The
patched tcd makes only the codecs in the mask.

Bit n of the mask is ECodecType n:

| Bit | Value | Codec field |
|---|---|---|
| 1 | `0x02` | dstar |
| 2 | `0x04` | dmr |
| 3 | `0x08` | m17 (c2_1600 selects the m17 field) |
| 4 | `0x10` | m17 (c2_3200) |
| 5 | `0x20` | p25 |
| 6 | `0x40` | usrp |

All codecs: `0x76`. A mask of `0`, or a mask with no known bit, means
all codecs.

From tcdpool to tcd, the mask is the request. tcd:

- decodes the input only when an output needs the PCM,
- encodes only the requested codecs,
- sends the reply when all requested fields are full,
- sets the fields that were not requested to 0,
- if only the input codec is requested, sends the reply at once with
  no work.

From tcd to tcdpool, the mask gives the fields that hold data.

tcdpool maps the codec set of the client to this mask. Measured in the
container (500 frames at 20 ms, speech, one process):

| Direction | Outputs | tcd CPU (s) | tcd latency avg (ms) |
|---|---|---|---|
| PCM to AMBE+2 | dmr only | 0.78 to 0.85 | 1.75 to 1.94 |
| PCM to AMBE+2 | all | 1.75 | 1.93 to 2.02 |
| AMBE+2 to PCM | pcm only | 0.35 to 0.38 | 0.93 to 0.99 |
| AMBE+2 to PCM | all | 1.38 to 1.49 | 1.67 to 1.80 |

## 6. Shared vocoder state in tcd

- AMBE+2 uses one md380 firmware emulator for the whole process. The
  test `md380_state` shows that encodes and decodes do not change each
  other, but two decode streams do: the decoder state is shared.
- The D-Star (mbe) and IMBE vocoder objects are also shared by all
  modules of one process.
- Thus one tcd process can carry only one voice stream at a time.
  tcdpool starts one process for each client session for this reason.

## 7. Test vectors

`testdata/tcd-wire-vectors.txt` has packets in raw and v2 form, with
the expected reply. `tests/tcd-wire/genvectors.cpp` makes the file from
the memory of the upstream struct. `tests/tcd-wire/codec_test.cpp`
(C++) and `internal/backend/nostar` (Go) check their encoders against
it. To run the C++ tests:

```
scripts/build-image.sh tcd-test
docker run --rm tcdpool-tcd-test:dev
```

The run also starts `wiretest` against the patched tcd (raw and v2)
and against the upstream tcd (raw). Failures with the upstream tcd are
expected. They show the bugs that the patches fix.
