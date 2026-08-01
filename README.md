# sendspin-streamer (Go)

A Sendspin `player@v1` client that re-broadcasts the received audio as endless
HTTP streams — one program, one binary, several output formats.

Built on the official SDK [`github.com/Sendspin/sendspin-go`](https://github.com/Sendspin/sendspin-go):
protocol, clock sync, scheduler and decoders (PCM/FLAC/Opus) come from the SDK.
The audio output is replaced by a custom `output.Output` implementation that
feeds a jitter buffer and all HTTP listeners instead of a sound card.

## Signal path

```text
Sendspin server ──ws──> sendspin-go receiver ──> decoder ──> hub
   (jitter buffer, volume, 20 ms tick, stereo s16 PCM)
      ├── /stream.mp3   MP3 encoder (libmp3lame) + optional ICY metadata
      ├── /stream.flac  native FLAC encoder (pure Go)
      ├── /stream.opus  Opus encoder (libopus) in a built-in Ogg muxer
      ├── /stream.wav   PCM s16le with a streaming WAV header
      └── /stream.pcm   raw PCM s16le
```

The encoders are modular: the hub only produces PCM, and **every listener gets
its own encoder instance** for the format it requested, so formats can be mixed
freely and a new format is just one `Format` entry in `encoder.go`. The tick
never stops: when the buffer is empty, silence is encoded so no stream breaks
and clients don't have to reconnect.

## Getting started

```bash
# Docker
docker compose up -d --build
# native
go build -o sendspin-streamer ./src && ./sendspin-streamer
```

## Streams

| URL | Content-Type | Codec |
| --- | --- | --- |
| `/stream.mp3` | `audio/mpeg` | MP3 (lossy, `MP3_BITRATE`), ICY metadata |
| `/stream.flac` | `audio/flac` | FLAC (lossless) |
| `/stream.opus` | `audio/ogg; codecs=opus` | Opus in Ogg (lossy, `MP3_BITRATE`) |
| `/stream.wav` | `audio/wav` | PCM s16le + streaming WAV header |
| `/stream.pcm` | `audio/L16` | raw PCM s16le, no container |

`/stream?format=flac` works as well; `/stream` serves `DEFAULT_FORMAT`.
Encoding is lazy: an encoder only runs while a client is actually connected to
that endpoint, so unused formats cost nothing. Which endpoints exist at all is
configured with `STREAM_FORMATS` (comma separated, `all` by default) — disabled
formats answer `404`. `/api/formats` lists every endpoint as JSON with its
`enabled`/`default` flags. Opus needs a source rate of
8/12/16/24/48 kHz, otherwise that endpoint answers `503`. If the source format
changes mid-stream, the response ends so the client reconnects with a fresh
container header.

- Status UI: `http://<host>:8000/`
- JSON: `http://<host>:8000/api/status`, health check: `/healthz`


## ICY metadata

The stream carries Shoutcast/Icecast headers (`icy-name`, `icy-description`,
`icy-genre`, `icy-br`, `icy-pub`) and **in-stream metadata**: when a client
sends `Icy-MetaData: 1`, the server answers with `icy-metaint` and injects a
`StreamTitle='Artist - Title';` block every `ICY_METAINT` bytes. The title comes
from the Sendspin metadata of the current track and is only re-sent when it
changes (empty blocks otherwise); without a track title the station name is used.
Clients that don't request metadata get a plain MP3 stream; the other formats carry
only the static ICY headers. `ICY_METAINT=0` disables in-stream metadata entirely.

```bash
curl -H "Icy-MetaData: 1" http://<host>:8000/stream.mp3 -o out.mp3
```


Native build dependencies: `gcc`, `lame-dev`, `opus-dev`, `opusfile-dev` (cgo).

## Configuration (ENV)

| Variable | Default | Meaning |
| --- | --- | --- |
| `NAME` | `Sendspin Streamer (<host>)` | Display name of the player |
| `SENDSPIN_SERVER` | – | `host:port` of the server; empty = mDNS discovery |
| `MDNS` | `1` | mDNS discovery and rediscovery on reconnect |
| `CLIENT_ID` | auto | Stable client ID |
| `HTTP_PORT` | `8000` | Port for all streams and status pages |
| `STREAM_FORMATS` | `all` | Formats to serve, e.g. `mp3,flac`; everything else returns 404 |
| `DEFAULT_FORMAT` | `mp3` (or first enabled) | Format served on `/stream` and in the status player |
| `MP3_BITRATE` | `192` | Bitrate in kbps for the lossy formats (MP3, Opus) |
| `BUFFER_MS` | `800` | Jitter buffer ahead of the encoders |
| `VOLUME` | `100` | Initial volume 0–100 |
| `ICY_METAINT` | `16000` | Byte interval between ICY metadata blocks (MP3); `0` disables them |
| `ICY_DESCRIPTION` | `Sendspin Streamer` | `icy-description` header |
| `ICY_GENRE` | `Various` | `icy-genre` header |

| `PREFERRED_CODEC` | `pcm` | `pcm`, `flac` or `opus` |
| `MAX_SAMPLE_RATE` | `48000` | Upper limit of the offered sample rate |
| `MAX_BIT_DEPTH` | `16` | Upper limit of the offered bit depth |

Reconnect with backoff is enabled; without a fixed server address, mDNS
discovery runs again before every attempt.
