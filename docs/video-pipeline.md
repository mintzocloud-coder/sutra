<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->

# Video pipeline (D4)

End-to-end path for video uploads: the API's media registry (D4a) +
the background transcode worker (D4b) turn a raw source clip into a
multi-rendition HLS package with a poster frame, stored on SeaweedFS.
Posts can only reference media whose row is `ready`, so a clip is never
visible before its playback package exists (the "ready media only" gate).

## Lifecycle

```
client                          API                          worker
  │                              │                               │
  │  POST /v1/media/upload       │                               │
  │  {kind: video}               │ media row → pending           │
  │ ◄──── upload_url + media_id  │ (presigned direct SeaweedFS PUT) │
  │                              │                               │
  │  PUT <upload_url> (the clip) │                               │
  │ ─────────────────────────► seaweedfs                        │
  │                              │                               │
  │  POST /v1/media/{id}/complete│ HEAD fid → bytes confirmed    │
  │                              │ media row → uploaded          │
  │                              │ XADD media:transcode {media_id}│
  │ ◄──── 200 uploaded           │                               │
  │                                                ┌────────────┘
  │                                                ▼
  │                              worker: download clip, ffprobe,
  │                              transcode ladder ≤ source,
  │                              upload outputs, MarkReady
  │                                                │
  │  GET /v1/media/{id}  ◄────── media row → ready ──┘
  │  {status: ready, renditions: [...], poster_url}
```

Media status values (`internal/media`):

| status     | meaning                                             |
|------------|-----------------------------------------------------|
| `pending`  | upload URL minted, no bytes yet                     |
| `uploaded` | bytes confirmed; a video is waiting on the worker   |
| `ready`    | playback package complete — safe to reference       |
| `failed`   | worker gave up (attempts exhausted); see `error`     |

Images skip the pipeline entirely: `complete` on an image flips it
straight to `ready` — nothing to transcode.

## The transcode worker

`cmd/transcode-worker` (packaged by `services/api/Dockerfile.worker`,
runs as the `transcode-worker` compose service). It owns job durability;
nothing else may acknowledge stream entries.

- **Consumption.** Reads the Valkey stream `media:transcode` with
  `XREADGROUP` in a consumer group (default `transcode-workers`, consumer
  name = hostname via `TRANSCODE_CONSUMER`, so N replicas divide jobs
  automatically). Messages are ACKed only after the job resolves
  (ready or dead-lettered) — a crashed worker leaves the job in the
  pending list for redelivery, never lost.
- **Retries.** Failures increment `media:transcode:attempts` (a Valkey
  hash keyed by media id) and requeue with exponential backoff —
  base `TRANSCODE_BACKOFF_BASE` (30s) capped at `TRANSCODE_BACKOFF_MAX`
  (30m). After `TRANSCODE_MAX_ATTEMPTS` (default 5) the job is parked
  on the `media:transcode:dead` stream and the row is marked `failed`
  with the error text — permanent, visible, debuggable; re-running is a
  deliberate operator act, not a silent loop.
- **Duplicate deliveries** (a job redelivered after a crash) are no-ops:
  the worker skips any job whose row is already `ready` or `failed`.
- **Stuck sweep.** On startup and every `TRANSCODE_SWEEP_INTERVAL` the
  worker re-enqueues `uploaded` rows that never made it onto the stream
  (e.g. the API died between the status flip and the XADD). Belt and
  suspenders around the crash window.
- **Liveness.** Each loop TTL-refreshes `worker:transcode:heartbeat`
  (TTL = 3× the block window). Orchestrators probe that key; a worker
  that is alive-but-stalled stops refreshing, which a process check
  would miss. The image HEALTHCHECK (`pidof`) is only the backstop.
- **Fail-closed.** Like the API it refuses to boot without PostgreSQL
  and Valkey. SeaweedFS reachability is warn-only at startup — a dead
  volume server fails individual jobs through the retry/dead-letter
  machinery instead of killing the whole worker.

## Transcode ladder

`ffprobe` reads the source resolution first; the ladder is the default
rungs **at or below** it — never upscale (a 480p source does not get a
720p rendition, it would just waste bits):

| name | size        | video   | audio  | bandwidth  |
|------|-------------|---------|--------|------------|
| 720p | 1280×720    | 2500k   | 128k   | 2.8 Mbit/s |
| 480p | 854×480     | 1200k   | 128k   | 1.4 Mbit/s |
| 360p | 640×360     | 800k    | 96k    | 0.9 Mbit/s |

(Above 720p we do not generate yet: the bitrate cost of 1080p+ on
user-generated mobile video rarely buys visible quality over a well-
encoded 720p, and Wave 1 favors reliability. 1080p is a one-line rung
addition in `DefaultLadder` when demand proves it.)

A source narrower than the smallest rung still yields a rendition of
itself — every video gets a playable package, never an empty one.

## Outputs

Per job, in a working dir on the worker's disk (`TMPDIR`):

- one HLS variant per rung: `<name>.m3u8` + `<name>_NNN.ts` segments
  (4s segments, VOD playlists — seekable, CDN-cacheable),
- `poster.jpg`: a single poster frame extracted from the source.

Uploaded back to SeaweedFS in order: poster first (its fid becomes
`poster_fid` on the row), then the HLS tree. `MarkReady` records
`renditions` (jsonb: name, size, bitrates, bandwidth, playlist fid)
plus the source `width`/`height`/`duration_ms` from ffprobe.

## Failure modes

| failure                          | behavior                                  |
|----------------------------------|-------------------------------------------|
| ffprobe finds no video stream    | job fails, retries, then dead-letters     |
| ffmpeg dies mid-encode           | same; partial local files are discarded   |
| SeaweedFS down during download   | job fails into backoff, recovers on boot  |
| API died before XADD             | stuck sweep re-enqueues on next pass      |
| worker crashes mid-job           | stream pending-list redelivery, idempotent |
| row already ready/failed         | redelivery is a no-op                     |

## Environment reference

All `TRANSCODE_*` keys live in `internal/config` with sane defaults;
the compose service sets only the connectivity trio (DB, Valkey,
SeaweedFS) and leaves tuning to the operator.

## Operations

- **Scale:** add `transcode-worker` replicas — the consumer group
  shares jobs; do not point two workers at the same consumer name.
- **Watch:** `XLEN media:transcode` (backlog), `XLEN
  media:transcode:dead` (permanent failures — investigate),
  `HGETALL media:transcode:attempts` (hot spots), the heartbeat key
  (worker health).
- **Requeue a dead job:** push a fresh entry onto `media:transcode`
  and reset the row to `uploaded` — a deliberate, logged operator act.
