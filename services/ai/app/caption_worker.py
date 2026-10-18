# SPDX-License-Identifier: AGPL-3.0-or-later
"""faster-whisper caption worker (E3 part 2).

Drains the Valkey stream ``media:captions`` (consumer group
``caption-workers``) and, per job:

1. Claims the media row (``caption_status: requested/failed -> processing``).
   0 rows affected = duplicate delivery -> ACK, no-op.
2. Downloads the source video from SeaweedFS.
3. Transcribes with faster-whisper (CPU, language auto-detect).
4. Writes ``captions.vtt`` + ``captions.srt`` sidecars back to SeaweedFS.
5. Resolves the row to ``caption_status=ready`` (the Go API then serves
   ``caption_url``), clears the attempts counter, ACKs the stream entry.

Failures: attempts are counted in a Valkey hash; a failed job is
re-queued after exponential backoff, and after CAPTION_MAX_ATTEMPTS it
goes to the ``media:captions:dlq`` stream AND the row is parked
``caption_status=failed`` with ``caption_error`` — nothing fails
silently. A periodic sweep re-drives rows stuck in ``requested`` (>10m,
lost XADD) or ``processing`` (>30m, crashed worker) — the claim UPDATE
makes the sweep race-safe.

Dependencies are constructor-injected (``Storage`` / ``Transcriber`` /
``StreamOps``) so the whole lifecycle is unit-testable with fakes; the
production wiring lives in app/caption_main.py. faster-whisper is
imported lazily inside FasterWhisperTranscriber so module import never
pays the model-library cost (and tests run without it installed).
"""
from __future__ import annotations

import logging
import time
from pathlib import Path
from typing import Protocol

import httpx

from app import captions

log = logging.getLogger("sutra-ai.captions")


class SeaweedUnavailable(Exception):
    """SeaweedFS master/volume unreachable — the job must be retried,
    never parked failed on a storage outage."""


class Storage(Protocol):
    """Bytes in/out of SeaweedFS."""

    def download(self, fid: str) -> bytes: ...
    def upload(self, filename: str, mime: str, data: bytes) -> str: ...


class Transcriber(Protocol):
    """Audio -> caption cues. Implementations: faster-whisper, test stub."""

    def transcribe(self, path: str) -> list[captions.CaptionSegment]: ...
    @property
    def detected_language(self) -> str: ...


class StreamMessage:
    """One stream entry the worker read."""

    def __init__(self, entry_id: str, values: dict):
        self.entry_id = entry_id
        self.values = values


class StreamOps(Protocol):
    """The minimal Valkey surface the worker needs (mirrors the Go
    transcode worker's streamOps so the two playbooks stay comparable)."""

    def ensure_group(self) -> None: ...
    def read_group(self, block_ms: int) -> list[StreamMessage]: ...
    def ack(self, entry_id: str) -> None: ...
    def add(self, values: dict) -> str: ...
    def get_attempts(self, media_id: str) -> int: ...
    def set_attempts(self, media_id: str, n: int) -> None: ...
    def clear_attempts(self, media_id: str) -> None: ...
    def heartbeat(self) -> None: ...


class RedisStreamOps:
    """StreamOps over the ``redis`` package (RESP — works against the
    Valkey 8 the compose stack runs, same as go-redis on the Go side)."""

    def __init__(self, addr: str, settings: captions.CaptionSettings):
        import redis

        host, _, port = addr.partition(":")
        self._r = redis.Redis(
            host=host or "valkey",
            port=int(port or 6379),
            decode_responses=True,
            socket_connect_timeout=5,
            socket_timeout=10,
        )
        self._settings = settings

    def ensure_group(self) -> None:
        try:
            self._r.xgroup_create(
                captions.STREAM,
                self._settings.group,
                id="0",
                mkstream=True,
            )
        except Exception as exc:  # noqa: BLE001 - redis-py ResponseError
            if "BUSYGROUP" not in str(exc):
                raise

    def read_group(self, block_ms: int) -> list[StreamMessage]:
        res = self._r.xreadgroup(
            self._settings.group,
            self._settings.consumer,
            {captions.STREAM: ">"},
            count=1,
            block=block_ms,
        )
        out: list[StreamMessage] = []
        for _stream, messages in res or []:
            for entry_id, values in messages:
                out.append(StreamMessage(entry_id, dict(values)))
        return out

    def ack(self, entry_id: str) -> None:
        self._r.xack(captions.STREAM, self._settings.group, entry_id)

    def add(self, values: dict) -> str:
        return str(self._r.xadd(captions.STREAM, values))

    def dead_letter(self, values: dict) -> str:
        return str(self._r.xadd(captions.DEAD_LETTER_STREAM, values))

    def get_attempts(self, media_id: str) -> int:
        raw = self._r.hget(captions.ATTEMPTS_HASH, media_id)
        if raw is None:
            return 0
        try:
            return int(raw)
        except (TypeError, ValueError):
            return 0  # corrupt counter: treat as fresh, never wedge a job

    def set_attempts(self, media_id: str, n: int) -> None:
        self._r.hset(captions.ATTEMPTS_HASH, media_id, n)

    def clear_attempts(self, media_id: str) -> None:
        self._r.hdel(captions.ATTEMPTS_HASH, media_id)

    def heartbeat(self) -> None:
        self._r.set(
            captions.HEARTBEAT_KEY,
            time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            ex=int(self._settings.heartbeat_ttl_seconds),
        )


class SeaweedStorage:
    """SeaweedFS over HTTP: reads via the volume base URL, writes via
    master dir/assign + direct PUT — the same contract as the Go API's
    media service and the transcode worker's seaweedStorage."""

    def __init__(
        self,
        master_addr: str,
        public_base_url: str,
        timeout: float = 30.0,
    ):
        self._master = master_addr
        self._base = public_base_url.rstrip("/")
        self._client = httpx.Client(timeout=timeout)

    def download(self, fid: str) -> bytes:
        try:
            resp = self._client.get(f"{self._base}/{fid}")
        except httpx.HTTPError as exc:
            raise SeaweedUnavailable(f"volume GET failed: {exc}") from exc
        if resp.status_code != 200:
            raise SeaweedUnavailable(
                f"volume GET {fid}: status {resp.status_code}"
            )
        return resp.content

    def upload(self, filename: str, mime: str, data: bytes) -> str:
        try:
            aresp = self._client.get(f"http://{self._master}/dir/assign")
        except httpx.HTTPError as exc:
            raise SeaweedUnavailable(f"dir/assign failed: {exc}") from exc
        if aresp.status_code != 200:
            raise SeaweedUnavailable(
                f"dir/assign: status {aresp.status_code}"
            )
        try:
            assign = aresp.json()
            fid, url = assign["fid"], assign["url"]
        except (ValueError, KeyError) as exc:
            raise SeaweedUnavailable(f"dir/assign: bad payload: {exc}") from exc
        if not fid or not url:
            raise SeaweedUnavailable("dir/assign: empty fid/url")
        try:
            presp = self._client.put(
                f"http://{url}/{fid}",
                content=data,
                headers={"Content-Type": mime},
            )
        except httpx.HTTPError as exc:
            raise SeaweedUnavailable(f"volume PUT failed: {exc}") from exc
        if presp.status_code not in (200, 201):
            raise SeaweedUnavailable(f"volume PUT: status {presp.status_code}")
        return str(fid)


class FasterWhisperTranscriber:
    """Production transcriber. The faster_whisper import and the model
    load happen in __init__ (called once at worker startup), never at
    module import — importing this module stays cheap."""

    def __init__(self, settings: captions.CaptionSettings):
        from faster_whisper import WhisperModel

        model_dir = Path(settings.model_dir)
        model_dir.mkdir(parents=True, exist_ok=True)
        log.info(
            "loading faster-whisper model %r (cpu, int8) into %s",
            settings.model,
            model_dir,
        )
        self._model = WhisperModel(
            settings.model,
            device="cpu",
            compute_type="int8",
            download_root=str(model_dir),
        )
        self._language = ""

    @property
    def detected_language(self) -> str:
        return self._language

    def transcribe(self, path: str) -> list[captions.CaptionSegment]:
        segments_iter, info = self._model.transcribe(path, language=None)
        self._language = info.language or ""
        out: list[captions.CaptionSegment] = []
        for seg in segments_iter:
            text = (seg.text or "").strip()
            if not text:
                continue
            out.append(
                captions.CaptionSegment(start=seg.start, end=seg.end, text=text)
            )
        log.info(
            "transcribed %s: %d cues, language=%s, duration=%.1fs",
            path,
            len(out),
            self._language,
            info.duration,
        )
        return out


class CaptionWorker:
    """Drains media:captions and resolves caption jobs. One job at a
    time; run() returns when stop() is called, finishing the in-flight
    job first (graceful shutdown)."""

    def __init__(
        self,
        pool,  # psycopg_pool.ConnectionPool
        stream: StreamOps,
        storage: Storage,
        transcriber: Transcriber,
        settings: captions.CaptionSettings,
    ):
        self._pool = pool
        self._stream = stream
        self._storage = storage
        self._transcriber = transcriber
        self._settings = settings
        self._stopped = False
        self._last_sweep = 0.0
        Path(settings.work_dir).mkdir(parents=True, exist_ok=True)

    def stop(self) -> None:
        self._stopped = True

    # -- SQL helpers ------------------------------------------------------

    def _claim(self, media_id: str) -> bool:
        with self._pool.connection() as conn, conn.cursor() as cur:
            cur.execute(
                captions.CLAIM_JOB_SQL,
                (
                    captions.STATUS_PROCESSING,
                    media_id,
                    captions.STATUS_REQUESTED,
                    captions.STATUS_FAILED,
                ),
            )
            return cur.rowcount > 0

    def _resolve(self, result: captions.CaptionResult) -> None:
        with self._pool.connection() as conn, conn.cursor() as cur:
            cur.execute(
                captions.RESOLVE_JOB_SQL,
                (
                    captions.STATUS_READY,
                    result.vtt_fid,
                    result.srt_fid,
                    result.media_id,
                    captions.STATUS_PROCESSING,
                ),
            )

    def _park_failed(self, media_id: str, reason: str) -> None:
        with self._pool.connection() as conn, conn.cursor() as cur:
            cur.execute(
                captions.FAIL_JOB_SQL,
                (captions.STATUS_FAILED, reason[:500], media_id),
            )

    def _source_fid(self, media_id: str) -> str | None:
        """The original upload fid (renditions are for playback; the
        source has the audio). fids is a text[]; the first entry is the
        source."""
        with self._pool.connection() as conn, conn.cursor() as cur:
            cur.execute(captions.JOB_LOOKUP_SQL, (media_id,))
            row = cur.fetchone()
        if not row or not row[1]:
            return None
        return row[1][0]

    # -- job lifecycle ----------------------------------------------------

    def _fail_job(self, entry_id: str, media_id: str, reason: str) -> None:
        attempts = self._stream.get_attempts(media_id) + 1
        if attempts >= self._settings.max_attempts:
            self._park_failed(media_id, reason)
            self._stream.clear_attempts(media_id)
            if hasattr(self._stream, "dead_letter"):
                self._stream.dead_letter(
                    {
                        "media_id": media_id,
                        "error": reason[:500],
                        "attempts": str(attempts),
                    }
                )
            log.error(
                "caption job %s dead-lettered after %d attempts: %s",
                media_id,
                attempts,
                reason,
            )
        else:
            self._stream.set_attempts(media_id, attempts)
            delay = captions.backoff_for(
                attempts,
                self._settings.backoff_base_seconds,
                self._settings.backoff_max_seconds,
            )
            log.warning(
                "caption job %s failed (attempt %d/%d): %s — retry in %.0fs",
                media_id,
                attempts,
                self._settings.max_attempts,
                reason,
                delay,
            )
            time.sleep(delay)
            self._stream.add({"media_id": media_id})
        self._stream.ack(entry_id)

    def process_message(self, msg: StreamMessage) -> None:
        job = captions.parse_job(msg.values)
        if job is None:
            log.warning(
                "dropping malformed caption job %s: %r", msg.entry_id, msg.values
            )
            self._stream.ack(msg.entry_id)
            return

        if not self._claim(job.media_id):
            # Duplicate delivery (redelivery, sweep re-drive racing a live
            # worker): someone else holds it. ACK the duplicate.
            log.info(
                "caption job %s already claimed — duplicate delivery, ACK",
                job.media_id,
            )
            self._stream.ack(msg.entry_id)
            return

        log.info("caption job %s claimed, transcribing", job.media_id)
        try:
            result = self._transcribe_job(job.media_id)
        except SeaweedUnavailable as exc:
            # Storage outage: requeue WITHOUT counting an attempt — the
            # job is fine, the backend is down. Release the claim so the
            # sweep, not a crash loop, owns the retry.
            log.warning("seaweedfs unavailable for %s: %s", job.media_id, exc)
            self._release_claim(job.media_id)
            time.sleep(self._settings.backoff_base_seconds)
            self._stream.add({"media_id": job.media_id})
            self._stream.ack(msg.entry_id)
            return
        except Exception as exc:  # noqa: BLE001 - any job failure is counted
            self._fail_job(msg.entry_id, job.media_id, f"{type(exc).__name__}: {exc}")
            return

        self._resolve(result)
        self._stream.clear_attempts(job.media_id)
        self._stream.ack(msg.entry_id)
        log.info(
            "caption job %s ready: %d cues (%s + %s), lang=%s",
            job.media_id,
            result.segment_count,
            result.vtt_fid,
            result.srt_fid,
            result.detected_language,
        )

    def _release_claim(self, media_id: str) -> None:
        """Drop a processing claim back to requested (used on storage
        outages so the job is re-drivable, never stuck)."""
        with self._pool.connection() as conn, conn.cursor() as cur:
            cur.execute(
                captions.REDRIVE_PROCESSING_SQL,
                (
                    captions.STATUS_REQUESTED,
                    media_id,
                    captions.STATUS_PROCESSING,
                ),
            )

    def _transcribe_job(self, media_id: str) -> captions.CaptionResult:
        fid = self._source_fid(media_id)
        if not fid:
            raise ValueError("no source fid on media row")
        data = self._storage.download(fid)
        work = Path(self._settings.work_dir) / f"{media_id}.mp4"
        work.write_bytes(data)
        try:
            segments = self._transcriber.transcribe(str(work))
            vtt = captions.format_vtt(segments).encode("utf-8")
            srt = captions.format_srt(segments).encode("utf-8")
            vtt_fid = self._storage.upload("captions.vtt", "text/vtt", vtt)
            srt_fid = self._storage.upload("captions.srt", "application/x-subrip", srt)
        finally:
            work.unlink(missing_ok=True)
        return captions.CaptionResult(
            media_id=media_id,
            vtt_fid=vtt_fid,
            srt_fid=srt_fid,
            segment_count=len(segments),
            detected_language=self._transcriber.detected_language,
        )

    # -- sweep ------------------------------------------------------------

    def sweep(self) -> None:
        """Durability backstop: re-drive rows whose XADD was lost
        (requested, untouched >10m) and rows whose worker died mid-job
        (processing, untouched >30m — re-driven as requested; the claim
        gate makes this safe)."""
        with self._pool.connection() as conn, conn.cursor() as cur:
            cur.execute(
                captions.SWEEP_REQUESTED_SQL,
                (captions.STATUS_REQUESTED, self._settings.requested_stuck_seconds),
            )
            stuck_requested = [row[0] for row in cur.fetchall()]
            cur.execute(
                captions.SWEEP_PROCESSING_SQL,
                (captions.STATUS_PROCESSING, self._settings.processing_stuck_seconds),
            )
            stuck_processing = [row[0] for row in cur.fetchall()]
        for media_id in stuck_requested:
            log.warning("sweep: re-driving stuck requested caption job %s", media_id)
            self._stream.add({"media_id": str(media_id)})
        for media_id in stuck_processing:
            with self._pool.connection() as conn, conn.cursor() as cur:
                cur.execute(
                    captions.REDRIVE_PROCESSING_SQL,
                    (
                        captions.STATUS_REQUESTED,
                        str(media_id),
                        captions.STATUS_PROCESSING,
                    ),
                )
                if cur.rowcount > 0:
                    log.warning(
                        "sweep: crashed worker claim released for %s", media_id
                    )
                    self._stream.add({"media_id": str(media_id)})

    # -- main loop --------------------------------------------------------

    def run(self) -> None:
        self._stream.ensure_group()
        log.info(
            "caption worker up: model=%s group=%s consumer=%s",
            self._settings.model,
            self._settings.group,
            self._settings.consumer,
        )
        while not self._stopped:
            try:
                self._stream.heartbeat()
                now = time.monotonic()
                if now - self._last_sweep >= self._settings.sweep_interval_seconds:
                    self.sweep()
                    self._last_sweep = now
                for msg in self._stream.read_group(block_ms=5000):
                    if self._stopped:
                        break
                    self.process_message(msg)
            except KeyboardInterrupt:
                break
            except Exception as exc:  # noqa: BLE001 - loop must not die
                log.exception("caption worker loop error: %s", exc)
                time.sleep(5)
        log.info("caption worker stopped")
