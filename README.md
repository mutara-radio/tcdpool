# tcdpool

tcdpool is a server that gives digital voice transcoding to remote
clients. It runs a pool of [nostar tcd](https://github.com/nostar/tcd)
transcoder processes (branch `tcd-nohw`, software vocoders only) and
gives each client session its own tcd process.

- Clients connect over WebSocket with TLS and send a key in the
  `Authorization` header.
- Clients send 20 ms voice frames and get the transcoded frames back.
  Each frame says which output codecs it needs. tcd makes only those
  codecs.
- Codecs: PCM (8 kHz, 16-bit), D-Star AMBE, AMBE+2 (DMR, YSF, NXDN),
  P25 IMBE and Codec2 3200.
- tcdpool stops a tcd process when its session has had no connection
  for 60 s. A client that connects again in this time with the same
  key and session name gets the same process.
- tcdpool starts a failed tcd process again, with a backoff, and tells
  the client.
- Each key and the host have a maximum number of processes.
- Each process has stats: frames, latency, late and missing frames,
  restarts and CPU time. The client can ask for them, and the admin
  endpoint on loopback shows them.
- A client can read `GET /v1/status` with its key: the load of the
  server (ok, busy or full) and the sessions of its own key.

tcdpool is one static Go binary (no cgo). The image has tcdpool and a
patched tcd.

## Documents

| Document | Content |
|---|---|
| [docs/protocol.md](docs/protocol.md) | The client protocol: connection, handshake, frames, errors and golden bytes. Write a client from this document. |
| [docs/tcd-wire.md](docs/tcd-wire.md) | The link between tcdpool and tcd: the packet, raw and v2 framing, the outputs mask. |
| [patches/tcd-nohw](patches/tcd-nohw) | The patches to tcd, as `git format-patch` files. |

## Build

You need Docker (or Colima) and Go 1.27.

The tcd build needs the md380 SRAM core image `d02032-core.img` of
md380_vocoder_dynarmic. This repository does not have it (see
"Licences"). Give its path in `MD380_CORE`:

```
MD380_CORE=/path/to/d02032-core.img scripts/build-image.sh
```

This builds the image `tcdpool:dev` (target `final`). The script:

1. copies the core image to a build context,
2. makes a local Go module proxy from your module cache, so the image
   build downloads no Go code,
3. runs `docker build`.

The Dockerfile pins the base images by tag and digest, and the git
sources by commit:

| Source | Pin |
|---|---|
| `debian:trixie` | `sha256:913f6706df59a68922d1dd08f78c2476560a8d367897200a6005b00e5f67c2d5` |
| `debian:trixie-slim` | `sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f` |
| `golang:1.27.2-trixie` | `sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e` |
| nostar/tcd | `e347df69c0775a1f3e16298e098bfba313186ab6` (branch tcd-nohw) |
| dynarmic (yuzu-mirror) | `9d4582339990d4eae53f1dc7160686920fc2075c` |

The md380 build also downloads the TYT MD-380 firmware 2.32 from
md380.org and checks its SHA-256.

The final image runs as the user `tcdpool` (uid 10001). It has
`/opt/tcdpool/tcdpool`, `/opt/tcdpool/tcd`,
`/opt/tcdpool/tcdpool-client` and the config
`/etc/tcdpool/tcdpool.json`.

To build only the Go programs:

```
go build ./cmd/tcdpool ./cmd/tcdpool-client
```

### Tests

```
go vet ./... && go test ./...
```

The Go tests use a fake tcd (`internal/faketcd`). It can crash, hang,
send bad data and send late replies on command.

The C++ link tests run in the `tcd-test` image:

```
scripts/build-image.sh tcd-test
docker run --rm tcdpool-tcd-test:dev
```

## Configuration

The config file is JSON. Durations are strings such as `"60s"` or
`"500ms"`. An unknown field is an error. See
[deploy/tcdpool.example.json](deploy/tcdpool.example.json).

| Field | Default | Meaning |
|---|---|---|
| `listen` | `:8443` | The address for clients. |
| `tls.cert`, `tls.key` | - | The certificate and key files (PEM). |
| `tls.self_signed` | `false` | Make a new self-signed certificate at each start. For development only. The log gives its SHA-256 for pinning. |
| `tls.disable` | `false` | Plain WebSocket. Use only behind a TLS proxy. |
| `admin_listen` | `127.0.0.1:9180` | The admin endpoint. It must be a loopback address. Empty turns it off. |
| `keys_file` | `keys.json` | The key file. |
| `backend.binary` | `tcd` | The path of tcd. |
| `backend.work_dir` | system temp | tcdpool writes the `tcd.ini` files here (mode 0600). |
| `backend.port_min`, `port_max` | `40000`, `48999` | The loopback ports for the tcd links. |
| `backend.framing` | `v2` | `v2` (needs the patched tcd) or `raw` (upstream format). |
| `backend.gains` | all `0` | tcd gains in dB (-24..24): `dstar_in`, `dstar_out`, `dmr_in`, `dmr_out`, `usrp_tx`, `usrp_rx`. `0` is neutral. |
| `backend.connect_timeout` | `15s` | The time tcd gets to start and connect. |
| `backend.watchdog` | `20s` | No data from tcd for this time is a stall. |
| `idle_timeout` | `60s` | Stop a process when its session has had no connection for this time. |
| `max_processes` | `8` | The maximum number of tcd processes on the host. |
| `key_max_processes` | `2` | The maximum for a key that has no limit of its own. |
| `late_after` | `60ms` | A reply that takes longer is late. |
| `missing_after` | `2s` | A frame with no reply after this time is missing. |
| `restart.min_backoff`, `max_backoff` | `500ms`, `30s` | The wait before a restart. It doubles after each failure in a row. |
| `restart.max_failures` | `5` | Give up after more restarts in a row with no good reply. |

Check a config file:

```
tcdpool check-config -config tcdpool.json
```

Start the server:

```
tcdpool serve -config tcdpool.json
```

With Docker, the image config keeps the keys in `/var/lib/tcdpool`.
Make the first key in a volume, then start the server. Publish the
client port only. The admin port stays on loopback in the container;
use `docker exec` to read it:

```
docker volume create tcdpool-data
docker run --rm -v tcdpool-data:/var/lib/tcdpool tcdpool:dev \
  key create -keys /var/lib/tcdpool/keys.json -label "N0CALL hub"
docker run -d --name tcdpool -p 127.0.0.1:8443:8443 \
  -v tcdpool-data:/var/lib/tcdpool tcdpool:dev
docker exec tcdpool tcdpool stats
```

To use other settings, put your own config file in the image or in a
volume, and give its path: `serve -config <path>`.

## Key management

A key has an ID, a SHA-256 hash of its secret, a label, an enabled flag
and an optional maximum number of processes. The key file does not
contain secrets. tcdpool writes it with mode 0600 and reads it again
when it changes (about each 2 s). tcdpool never writes a secret to its
log.

Make a key. The command shows the token one time only:

```
tcdpool key create -keys keys.json -label "N0CALL hub" -max-processes 2
```

The client sends the token as `Authorization: Bearer <token>`.

Other commands:

```
tcdpool key list -keys keys.json
tcdpool key disable -keys keys.json k3f9a0c12be47
tcdpool key enable -keys keys.json k3f9a0c12be47
```

When you disable or remove a key, the server stops its processes and
closes its connections with the error `key_revoked`.

## Limits and stats

- `max_processes` limits the tcd processes on the host. A client gets
  `host_full` when the host is full.
- Each key has a limit. A client gets `key_limit` when its key is at
  the limit. The error has `in_use` and `max`, for example
  `no free session for this key: 2 of 2 in use`.
- To make room, the server stops the process that has been idle for
  the longest time. It never stops a process that has a connection.

Client status, on the client port with the client key (see
[docs/protocol.md](docs/protocol.md) section 12):

```
curl -sk -H "Authorization: Bearer $TOKEN" https://127.0.0.1:8443/v1/status
```

It shows the version, the processes of the host and their limit, the
sessions of the caller's key and their limit, the CPU load (Linux),
the late rate and a state: `ok`, `busy` (80 % of the processes in use,
CPU load 0.8 for each CPU, or 2 % late replies) or `full` (no free
process). It never shows other keys or session names.

Admin endpoint (loopback only):

| Path | Content |
|---|---|
| `/healthz` | `ok` |
| `/stats` | JSON: all processes with their stats. |
| `/metrics` | Prometheus text format. |

```
tcdpool stats -admin 127.0.0.1:9180
```

## Test client

`tcdpool-client` sends test signals (a 1 kHz tone, a 440 Hz tone and a
speech-like signal) as PCM, gets them back as a codec, sends the codec
frames back and gets PCM again. It paces frames at 20 ms and shows
latency (min, average, p99, max), late and missing frames, the tcd CPU
time and an audio check (energy ratio, envelope correlation, tone
purity). It can write WAV files.

```
TCDPOOL_KEY=<token> tcdpool-client -url wss://127.0.0.1:8443/v1/stream \
  -pin <sha256> -codecs dmr,dstar,codec2_3200 -out ./wav
```

Use `-parallel N` for N sessions at the same time, and `-all-outputs`
to ask for all codecs (to compare with one codec).

Vocoders model speech. A pure tone can come back with a different
spectrum. The client shows a warning for a tone, not a failure.

Results on an Apple silicon host, in Colima (4 CPUs), one session, 250
frames of speech at 20 ms (times in ms):

| Stage | Client round trip avg / p99 | tcd avg / p99 | Audio |
|---|---|---|---|
| PCM to AMBE+2 | 3.77 / 5.97 | 1.86 / 3.32 | envelope 0.82, energy 0.96 |
| AMBE+2 to PCM | 3.04 / 5.45 | 0.92 / 1.99 | |
| PCM to D-Star | 3.37 / 5.62 | 1.16 / 2.10 | envelope 0.82, energy 0.39 |
| D-Star to PCM | 3.06 / 4.94 | 0.92 / 2.13 | |
| PCM to Codec2 3200 | 3.18 / 5.61 | 1.12 / 1.83 | envelope 0.88, energy 1.22 |
| Codec2 3200 to PCM | 2.95 / 5.42 | 0.90 / 1.45 | |

There were no late and no missing frames.

## Licences

tcdpool's own code is in this repository. The image also contains
third-party code with its own licences. **The person or group that
builds, runs or distributes tcdpool or its image must decide if they
can use these vocoders and obey these licences. The vocoder licence
questions stay with them.** AMBE and AMBE+2 are subject to patents and
licence claims of Digital Voice Systems, Inc. in some places.

| Part | Where | Licence |
|---|---|---|
| tcd (nostar) | downloaded at build, patched by `patches/tcd-nohw` | GPL-3.0 |
| tcd patches | `patches/tcd-nohw` | GPL-3.0 (the licence of tcd) |
| Codec2 (in tcd, `codec2/`) | downloaded with tcd | LGPL-2.1 |
| mbe vocoder (in tcd, `mbe/`; D-Star AMBE) | downloaded with tcd | ISC |
| IMBE vocoder (OP25) | `third_party/imbe_vocoder` | GPL-3.0-or-later |
| md380_vocoder_dynarmic (AMBE+2) | `third_party/md380_vocoder_dynarmic` | GPL-3.0 (LICENSE); some headers GPL-2.0-or-later |
| dynarmic | downloaded at build | 0BSD; its bundled libraries are MIT or BSD |
| TYT MD-380 firmware 2.32 (runs in the AMBE+2 emulator) | downloaded at build from md380.org; the SRAM core image `d02032-core.img` comes from the builder | Proprietary (TYT). Not in this repository. |
| Boost (headers, build only) | Debian package | BSL-1.0 |
| coder/websocket v1.8.15 | Go module | ISC |
| Debian base image | Docker Hub | Debian packages, many licences |

The files in `third_party` come from urfd commit
`172850c8888d9c7b301ec80c0fa15e3722bb15e3` (`src/imbe_vocoder` and
`src/md380_vocoder_dynarmic`), with no change.
