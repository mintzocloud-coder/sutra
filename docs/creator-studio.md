<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# Creator Studio — auto-captions (E3)

faster-whisper (MIT, CPU) transcribes ready videos; VTT + SRT sidecars
land on SeaweedFS; the media row exposes `caption_url`. The design
follows the transcode worker's (D4) playbook: stream-driven, consumer
group, backoff retries, dead-letter, sweep backstop — because a caption
job that silently vanishes is worse than no captions at all.

## Request paths (part 1 — implemented)

Three ways a video gets `auto_caption = true`:

1. **POST /v1/media/upload** intent with `auto_caption=true` *(planned;
   part 2 may add the flag to the upload intent if the studio UI needs
   it — today only the post path exists)*.
2. **POST /v1/posts** with `auto_caption=true`: after the 201, the
   `AfterCreateCaptions` hook calls `media.RequestCaptions` per video
   attachment — best-effort (logged, never fails the post), idempotent
   (client retries are safe).
3. Re-request any time: `RequestCaptions` is idempotent; a
   `caption_status` of `requested`/`processing`/`ready` is a no-op.

`RequestCaptions` enforces ownership and rejects images
(`ErrCaptionsVideoOnly` — nothing to transcribe). The state machine:

```
media.caption_status:  none ──→ requested ──→ processing ──→ ready
                         ↑          │              │
                         │          └──────┬───────┘
                         └────────────────┘ failed
```

- The row flips to `requested` only from `none`/`failed` (one atomic
  UPDATE — concurrent requests collapse into one job).
- If the video is already `ready`, the job is XADDed onto
  `media:captions` immediately.
- If it is still `uploaded`, the transcode worker calls
  `EnqueueCaptionsIfRequested` right after `MarkReady`; the guarded
  UPDATE (eligible: `auto_caption` + kind=video + status=ready +
  `caption_status IN (none, requested, failed)`) flips it and XADDs.
- The XADD is best-effort like the transcode path; the worker's sweep
  of `caption_status IN (requested, processing)` (index
  `media_caption_sweep_idx`) is the durability backstop.

## Caption worker (part 2 — implemented 2026-10-18)

Python worker in `services/ai` (same process family as the chat agent;
shares `app/db.py`'s pool + the advisory-locked migration runner):

- `app/captions.py` — domain logic: CAPTION_* settings, the SQL
  contract (claim/resolve/fail/sweep statements), WebVTT + SubRip
  formatters, exponential backoff, job parsing. Fully unit-tested
  (`tests/test_captions.py`, 14 tests).
- `app/caption_worker.py` — `CaptionWorker`: consumer-group drain,
  claim-then-transcribe, SeaweedFS round-trip (httpx), sidecar upload,
  resolve/fail/DLQ, stuck-row sweep, TTL heartbeat. Deps are
  constructor-injected (`Storage` / `Transcriber` / `StreamOps`), same
  interface/fake pattern as the Go transcode worker.
- `app/caption_main.py` — process entry point (fail-closed boot,
  SIGTERM/SIGINT drain); `Dockerfile.caption` (own image so the
  ~400MB whisper stack never bloats the FastAPI service); compose
  `caption-worker` service with the `whisper-models` named volume.
- `requirements-caption.txt` — caption-only pins
  (`redis==8.1.0`, `faster-whisper==1.2.1`) on top of the shared base.

- **Model:** faster-whisper, CPU, `base` (74M params — ~real-time on a
  modest CPU; ADR-006 notes ~6–7s per 15s clip). Model cache lives on a
  named volume so container restarts don't re-download.
- **Consume:** consumer group on `media:captions`
  (group `caption-workers`, consumer = hostname), 5s block; ACK after
  the row resolves (ready/failed) — same as the transcode worker.
- **Per job:**
  1. Claim the row: `UPDATE media SET caption_status='processing'
     WHERE id=$1 AND caption_status IN ('requested','failed')` — 0 rows
     means another worker took it (duplicate delivery → no-op).
  2. Download the source video from SeaweedFS (the original fid; the
     renditions are for playback, the source has the audio).
  3. Transcribe with faster-whisper (language auto-detect; Indic audio
     is the primary case — faster-whisper handles hi/gu/ta/bn/te/en).
  4. Write `captions.vtt` + `captions.srt` next to the video's fids on
     SeaweedFS (new fids via dir/assign).
  5. `UPDATE media SET caption_status='ready',
     caption_vtt_fid=$2, caption_srt_fid=$3, caption_error=NULL
     WHERE id=$1 AND caption_status='processing'` → GET /v1/media/{id}
     now serves `caption_url`.
- **Failures:** attempts counter (Valkey hash, same pattern as
  transcode), exponential backoff requeue, dead-letter stream
  `media:captions:dlq` + `caption_status='failed'` +
  `caption_error=<reason>` after `CAPTION_MAX_ATTEMPTS` (default 3 —
  transcription rarely recovers on retry; 3 keeps the queue honest).
- **Sweep:** every `CAPTION_SWEEP_INTERVAL` (default 5m), re-drive rows
  stuck in `requested` > 10m (no stream entry seen) — re-XADD once;
  rows stuck in `processing` > 30m are treated as crashed workers:
  re-drive as `requested` (the claim UPDATE makes this safe).
- **Liveness:** TTL heartbeat key `worker:captions:heartbeat` (same
  convention as `worker:transcode:heartbeat`).

The worker writes the media table directly (psycopg, same DATABASE_URL
as the ai service) — no new Go endpoints needed; the SQL above is the
contract. It never touches `posts`, `users`, or the chat service.

## Env (part 2)

| Var | Default | Notes |
|---|---|---|
| `CAPTION_MODEL` | `base` | faster-whisper model size (tiny/base/small) |
| `CAPTION_GROUP` | `caption-workers` | consumer group |
| `CAPTION_CONSUMER` | hostname | consumer id |
| `CAPTION_MAX_ATTEMPTS` | `3` | then dead-letter + failed |
| `CAPTION_BACKOFF_BASE` | `60s` | exponential backoff base |
| `CAPTION_BACKOFF_MAX` | `30m` | backoff cap |
| `CAPTION_SWEEP_INTERVAL` | `5m` | stuck-row sweep |
| `CAPTION_WORK_DIR` | `/tmp/captions` | download + sidecar scratch |

## Client contract

- `GET /v1/media/{id}` returns `caption_status` always, `caption_url`
  (VTT) when `ready`, `auto_caption` when opted in. `caption_error`
  stays internal — clients see the status, not the reason.
- Posting with `auto_caption=true` does not change the 201 semantics:
  captions arrive asynchronously; poll the media row.
- Web players: point `<track src=caption_url kind=captions>` at the VTT.
  The SRT twin is for download/editing clients.

## Smoke coverage

`scripts/smoke.sh` carries the full auto-caption round-trip (D4's clip
section): the caption worker is built and booted, gated on its TTL
heartbeat (`worker:captions:heartbeat`); the sample clip is minted with
a sine-wave audio track (faster-whisper needs an audio stream present —
`testsrc` is video-only); a post is created with `auto_caption=true`
→ `RequestCaptions` enqueues immediately (row already ready,
idempotent); the script polls `caption_status` to `ready` (300s
deadline) and asserts the VTT `caption_url` sidecar plus the sticky
`auto_caption`, then deletes the post. Skips cleanly where no host
ffmpeg exists (CI runners ship one). The transcode worker must have
turned the video `ready` first — the caption flow reuses that same
clip, so captions never run against a failed or missing transcode.

## Out of scope (Wave 2+)

- Caption *translation* (that's E4's IndicTrans2 bridge — it can MT the
  VTT text once it exists).
- Burned-in subtitles (ADR-006 lists FFmpeg burn-in as an option; the
  studio ships sidecars first — burn-in is a render-time choice).
- Speaker diarization, word-level timestamps (faster-whisper supports
  both; enable when the studio UI asks for karaoke-style captions).
- E2E-encrypted media: captions require server-side audio access, so
  captionable videos are necessarily non-E2E — consistent with the
  Wave-2 E2E boundary (E2E is text/1:1 only).
