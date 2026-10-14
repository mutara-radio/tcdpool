# tcdpool client protocol, version 1

This document gives all the data that you need to write a tcdpool
client. The Go code in `internal/protocol` uses the same rules, and its
tests use the golden bytes in section 9.

## 1. Overview

A client opens one WebSocket to tcdpool. The client sends voice frames
of 20 ms. tcdpool sends each frame to a tcd transcoder process and sends
the result back on the same WebSocket.

- One WebSocket is one session.
- One session has one tcd process. The key and the session name
  identify the process.
- Binary WebSocket messages carry voice frames.
- Text WebSocket messages carry control messages in JSON.
- All numbers in binary frames are little-endian.

## 2. Connect and authenticate

Open this URL:

```
wss://<host>:<port>/v1/stream
```

Send this HTTP header in the upgrade request:

```
Authorization: Bearer <key id>.<secret>
```

The operator gives you the token `<key id>.<secret>`. An example of the
form is `k3f9a0c12be47.Yp3...` (the key ID is `k` and 12 hex digits; the
secret is 43 characters of base64url).

If the server refuses the token, the upgrade fails with an HTTP status
and a JSON body:

| HTTP status | `code` | Meaning |
|---|---|---|
| 401 | `unauthorized` | No token, a bad token, an unknown key or a wrong secret. |
| 403 | `key_disabled` | The token is correct, but the operator disabled the key. |

Body example:

```json
{"type":"error","code":"unauthorized","message":"send Authorization: Bearer <key id>.<secret>","fatal":true}
```

Do not try again at once after a 401 or a 403. The result does not
change until the operator changes the key.

TLS: the server can use a self-signed certificate in development. The
server writes the SHA-256 of its certificate in its log. Pin that value
in the client, or use a certificate from a known CA. Do not turn off the
certificate check in production.

WebSocket settings:

- The server does not use compression (no `permessage-deflate`).
- The largest message from the client is 4096 bytes.

## 3. Session start

After the upgrade, send a `hello` text message. Send it in less than
5 s, or the server closes the connection.

```json
{"type":"hello","version":1,"session":"node-2100","outputs":["pcm"],"client":"mutara/1.4"}
```

| Field | Type | Rule |
|---|---|---|
| `type` | string | `"hello"` |
| `version` | int | `1` |
| `session` | string | 1 to 64 characters of `A-Z a-z 0-9 . _ : -`. |
| `outputs` | array of string | The default outputs (section 5). An empty array or no field means all codecs. |
| `client` | string | Optional. Free text for the server log. |

Codec names: `pcm`, `dstar`, `dmr`, `imbe`, `codec2_3200`. The server
also accepts these aliases: `usrp` (pcm), `ambe` (dstar), `ambe+2` and
`ysf` (dmr), `p25` (imbe), `codec2` and `c2` (codec2_3200).

The server then starts a tcd process, or it uses the process that the
key and the session name already have. When the process is ready, the
server sends `ready`:

```json
{"type":"ready","version":1,"session":"node-2100","reused":false,"restarted":false,
 "backend":"nostar-tcd","inputs":["pcm","dstar","dmr","imbe","codec2_3200"],
 "outputs":["pcm"],"frame_ms":20,"idle_timeout_s":60,"late_after_ms":60}
```

| Field | Meaning |
|---|---|
| `reused` | `true`: the process existed before this connection. The codec state continues. |
| `restarted` | `true`: the process failed and started again (section 7). |
| `inputs` | The codecs that you can send. |
| `outputs` | The default outputs of this session. |
| `idle_timeout_s` | The process stops this many seconds after the last connection of the session closes. |
| `late_after_ms` | A reply that takes longer than this has the late flag. |

The start of a new tcd process can take some seconds. Wait up to 30 s
for `ready`.

Do not send voice frames before `ready`. If the hello is bad, or if the
server cannot start a process, the server sends an `error` message
with `"fatal": true` and closes the WebSocket (section 8).

## 4. Voice frames

### 4.1 Request (client to server)

One binary message is one frame: a 12-byte header and one payload.

| Offset | Size | Field | Value |
|---|---|---|---|
| 0 | 1 | version | `1` |
| 1 | 1 | type | `1` (request) |
| 2 | 2 | stream | Stream ID, u16 LE. |
| 4 | 4 | seq | Sequence, u32 LE. |
| 8 | 1 | flags | Bit 0: last frame of the stream. Bits 1 to 7 must be 0. |
| 9 | 1 | in | Input codec ID (table 4.3). |
| 10 | 1 | outputs | Output codec set (table 4.3). `0` means the session default. |
| 11 | 1 | reserved | `0` |
| 12 | n | payload | Exactly the payload size of the input codec. |

The server refuses a frame that does not obey these rules. It sends an
`error` with code `bad_frame` and `"fatal": false` for the first three
bad frames of a connection, and then it is quiet. A bad frame gets no
reply.

### 4.2 Reply (server to client)

One binary message is one reply: a 16-byte header and the payloads.

| Offset | Size | Field | Value |
|---|---|---|---|
| 0 | 1 | version | `1` |
| 1 | 1 | type | `2` (reply) |
| 2 | 2 | stream | The stream ID of the request. |
| 4 | 4 | seq | The sequence of the request. |
| 8 | 1 | flags | Bit 0: last (a copy of the request flag). Bit 1: late. |
| 9 | 1 | in | The input codec of the request. |
| 10 | 1 | outputs | The set of payloads in this reply. |
| 11 | 1 | reserved | `0` |
| 12 | 4 | latency | Time in the transcoder, in microseconds, u32 LE. |
| 16 | n | payloads | One payload for each bit in `outputs`, in codec ID order. |

The latency is the time from the send to tcd to the reply from tcd. It
does not include the network between the client and tcdpool.

The late flag is set when the latency is more than `late_after_ms`.
A late reply is still correct. The client decides if it can use it.

### 4.3 Codecs

| ID | Name | Set bit | Payload | Content |
|---|---|---|---|---|
| 1 | `pcm` | `0x02` | 320 bytes | 160 samples, 8000 Hz, signed 16-bit LE, mono. |
| 2 | `dstar` | `0x04` | 9 bytes | D-Star AMBE (2400 bit/s voice with FEC, 3600 bit/s), as on the air. |
| 3 | `dmr` | `0x08` | 9 bytes | AMBE+2 (DMR, YSF DN, NXDN), 72 bits with FEC. |
| 4 | `imbe` | `0x10` | 11 bytes | P25 phase 1 IMBE, 88 bits. |
| 5 | `codec2_3200` | `0x20` | 8 bytes | One Codec2 3200 frame of 20 ms. |

The set bit of a codec is `1 << id`. Bits 0, 6 and 7 must be 0.

## 5. Which outputs you get

For each request, the server does these steps:

1. Take the `outputs` byte of the request. If it is `0`, take the
   session default from `hello`.
2. Remove the input codec. (You already have the input.)
3. Send the frame to tcd with this set. tcd makes only these codecs.
   It does not decode or encode the others.
4. Send the reply with these payloads.

If no codec is left after step 2, tcd does no work, and the reply has
`outputs = 0` and no payload. The reply still comes, so you can use it
as a check of the link.

Examples (session default `["pcm"]`):

| Request `in` | Request `outputs` | Reply `outputs` |
|---|---|---|
| dmr (3) | `0x00` | `0x02` (pcm) |
| pcm (1) | `0x08` | `0x08` (dmr) |
| pcm (1) | `0x24` | `0x24` (dstar and codec2_3200) |
| dmr (3) | `0x08` | `0x00` (no payload) |

Ask only for the codecs that you need. Each extra codec costs CPU. On
the test host, AMBE+2 to PCM used 3.9 times less CPU with
`outputs = pcm` than with all codecs, and PCM to AMBE+2 used 2.1 times
less CPU with `outputs = dmr` (see README.md).

## 6. Timing and matching

- Send each frame when you have it. Do not collect frames. tcdpool
  adds no buffer.
- One frame is 20 ms of audio. Send frames at the rate of the audio,
  one each 20 ms on average.
- Match each reply to its request by the pair (stream, seq). Do not
  use the order of arrival.
- Do not send a frame with the same (stream, seq) as a frame that has
  no reply yet. The server refuses it with a `bad_frame` error.
- Keep the sequence numbers of a stream continuous (0, 1, 2, ...).
  tcd uses the parity of the sequence for Codec2 frames.
- Use a new stream ID for each new voice stream. Set the last flag on
  the last frame of a stream.
- Ask for the same outputs for all frames of a stream.
- A frame with no reply after 2 s is missing. The server counts it.
  It does not send an error for it. A reply that comes after this
  time is dropped.
- A session can have at most 256 frames in the transcoder. The server
  refuses more with a `backlog` error (not fatal).
- The server has a send queue of 512 messages for each connection.
  If the client does not read fast enough, the server drops replies
  and counts them as `queue_dropped`.

The decoders and encoders in tcd keep state from frame to frame. Send
the frames of one stream in order, to one session. Do not send two
streams at the same time to one session: two decode streams in one tcd
process change the result of each other (the AMBE+2 decoder state is
shared). Use one session for each stream that can be active at the
same time.

## 7. Process failure and restart

If the tcd process stops, hangs or sends bad data, tcdpool starts it
again. The client stays connected.

1. The server sends `restarting`:
   ```json
   {"type":"restarting","reason":"process exited: signal: killed","retry_in_ms":500}
   ```
2. Frames that you send now get no reply.
3. When the new process is ready, the server sends `ready` with
   `"reused": true` and `"restarted": true`.
4. The codec state is new. Continue with the next frame.

In a test, the gap from the failure to the new `ready` was about
2.5 s, which includes the 500 ms wait. The wait doubles after each
failure in a row, up to 30 s. When the restarts in a row with no good
reply are more than `max_failures` (default 5), the server stops the
process and sends the fatal error `backend_failed`.

## 8. Control messages

### 8.1 From the client

| Message | Effect |
|---|---|
| `{"type":"stats"}` | The server sends a `stats` message (8.3). |
| `{"type":"bye"}` | End the session. The process stays for the idle time, so a new connection can use it. |
| `{"type":"bye","stop":true}` | End the session and stop the process now. |

The server ignores text messages that it does not know.

A normal close of the WebSocket has the same effect as `bye` without
`stop`.

### 8.2 Errors

```json
{"type":"error","code":"key_limit","message":"the key has its maximum number of processes (2)","fatal":true}
```

When `fatal` is `true`, the server closes the WebSocket after the
message, with the close code in this table.

| Code | Fatal | Close | Meaning | What to do |
|---|---|---|---|---|
| `bad_hello` | yes | 1008 | The first message is not a correct hello, or no hello in 5 s. | Fix the client. |
| `unsupported_version` | yes | 1008 | The `version` is not 1. | Fix the client. |
| `unsupported_codec` | yes | 1008 | An unknown codec in `outputs`. | Fix the client. |
| `key_limit` | yes | 1008 | The key has its maximum number of processes. | Close another session of the key, or wait. |
| `host_full` | yes | 1013 | The host has its maximum number of processes. | Try again later, or use another host. |
| `backend_start_failed` | yes | 1011 | tcd did not start. | Try again later. Tell the operator. |
| `backend_failed` | yes | 1011 | tcd failed too many times in a row. | Try again later. Tell the operator. |
| `replaced` | yes | 1008 | A new connection with the same key and session took the process. | Do not connect again automatically, or the two clients take the process from each other. |
| `key_revoked` | yes | 1008 | The operator disabled or removed the key. | Stop. |
| `shutting_down` | yes | 1001 | The server stops. | Connect again later. |
| `bad_frame` | no | - | A bad or duplicate voice frame. | Fix the client. |
| `backlog` | no | - | Too many frames in the transcoder. | Send at the audio rate. |

`unauthorized` and `key_disabled` come only as HTTP errors (section 2).

### 8.3 Stats

```json
{"type":"stats","id":"k3f9a0c12be47-node-2100-1","key_id":"k3f9a0c12be47",
 "label":"N0CALL test","session":"node-2100","backend":"nostar-tcd","pid":40,
 "state":"running","age_s":12.5,"uptime_s":12.4,"cpu_seconds":0.43,
 "starts":1,"crashes":0,"desyncs":0,"stalls":0,
 "frames_in":250,"frames_out":250,"late":0,"missing":0,"dropped":0,
 "unknown_replies":0,"send_failures":0,"pending":0,
 "latency":{"count":250,"min_ms":0.7,"avg_ms":1.99,"p50_ms":1.8,"p99_ms":11.71,"max_ms":39.59},
 "link":{"pings":2,"bad_frames":0,"skip_bytes":0},
 "queue_dropped":0}
```

The counters are for the process, from its first start. `latency` uses
the last 2048 replies. `state` is `running`, `idle`, `restarting` or
`stopped`. `crashes` counts process exits, `desyncs` counts bad data
from tcd, and `stalls` counts a tcd that sent nothing for 20 s.

## 9. Golden bytes

Request: stream `0x2101`, seq `0x01020304`, last, input dmr, outputs
pcm, payload `01 02 03 04 05 06 07 08 09`.

```
01 01 01 21 04 03 02 01 01 03 02 00 01 02 03 04 05 06 07 08 09
```

Reply: stream `0x2200`, seq 7, late, input pcm, outputs dstar and
codec2_3200 (`0x24`), latency 2500 us, dstar payload `d1 ... d9`,
codec2_3200 payload `c1 ... c8`.

```
01 02 00 22 07 00 00 00 02 01 24 00 c4 09 00 00
d1 d2 d3 d4 d5 d6 d7 d8 d9 c1 c2 c3 c4 c5 c6 c7 c8
```

## 10. Limits

- Each key has a maximum number of processes (the key setting, or the
  server default `key_max_processes`).
- The host has a maximum number of processes (`max_processes`).
- When a key is at its limit, the server stops the process of that key
  that has been idle for the longest time, if there is one.
- When the host is at its limit, the server stops the process on the
  host that has been idle for the longest time, if there is one.
- The server never stops a process that has a connection to make room.

## 11. Client checklist

1. Connect with the `Authorization` header. On 401 or 403, stop.
2. Send `hello` with a stable session name for each stream source.
3. Wait for `ready` (up to 30 s).
4. Send frames at the audio rate. Set `outputs` to only the codecs
   that you need.
5. Match replies by (stream, seq). Treat a frame with no reply after
   2 s as lost.
6. On `restarting`, expect no replies until the next `ready`.
7. On a fatal error, obey the table in 8.2 before you connect again.
   Use a backoff of at least 1 s, doubled after each failure.
8. At the end, send `bye`. Send `"stop": true` only when the session
   will not come back soon.
