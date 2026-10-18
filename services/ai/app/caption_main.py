# SPDX-License-Identifier: AGPL-3.0-or-later
"""Caption worker process entry point: ``python -m app.caption_main``.

Fail-closed startup, mirroring the FastAPI service's discipline
(app/config.py): DATABASE_URL is required (the worker resolves jobs by
writing the media table directly — a worker without the database would
consume jobs it can never finish), and the Valkey PING must succeed
before the consume loop starts. The faster-whisper model is loaded at
startup so a bad model name / missing cache fails the container fast,
not on the first user's video.
"""
from __future__ import annotations

import logging
import os
import signal
import sys

from app import captions, db
from app.caption_worker import (
    CaptionWorker,
    FasterWhisperTranscriber,
    RedisStreamOps,
    SeaweedStorage,
)

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(name)s %(levelname)s %(message)s",
)
log = logging.getLogger("sutra-ai.caption-main")


def main() -> int:
    settings = captions.CaptionSettings.from_env()

    database_url = os.environ.get("DATABASE_URL", "")
    if not database_url:
        log.error("DATABASE_URL is not set — refusing to start")
        return 1

    # Migrations are shared with the FastAPI service (same ai_ledger,
    # same advisory lock): whichever boots first applies them.
    db.run_migrations(database_url)
    pool = db.open_pool(database_url)

    stream = RedisStreamOps(settings.valkey_addr, settings)
    try:
        stream._r.ping()  # fail fast: no Valkey, no worker
    except Exception as exc:  # noqa: BLE001 - startup check
        log.error("valkey at %s unreachable: %s — refusing to start", settings.valkey_addr, exc)
        return 1

    storage = SeaweedStorage(
        settings.seaweedfs_master_addr, settings.media_public_base_url
    )
    try:
        transcriber = FasterWhisperTranscriber(settings)
    except Exception as exc:  # noqa: BLE001 - startup check
        log.error("faster-whisper model %r failed to load: %s", settings.model, exc)
        return 1

    worker = CaptionWorker(pool, stream, storage, transcriber, settings)

    def _handle(signum, _frame):  # noqa: ARG001
        log.info("signal %d received — draining in-flight job, then stopping", signum)
        worker.stop()

    signal.signal(signal.SIGTERM, _handle)
    signal.signal(signal.SIGINT, _handle)

    log.info(
        "caption worker starting (model=%s, group=%s, consumer=%s)",
        settings.model,
        settings.group,
        settings.consumer,
    )
    worker.run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
