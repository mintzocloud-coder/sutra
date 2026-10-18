# SPDX-License-Identifier: AGPL-3.0-or-later
# sutra-ai: Python intelligence layer (E1). Multi-stage: builder installs
# the pinned dependency tree, runtime carries only the venv + source.
# Runs as non-root; the Go API is the only caller (AI_SERVICE_TOKEN).

FROM python:3.12-slim AS builder
WORKDIR /build
COPY requirements.txt .
RUN pip install --no-cache-dir --prefix=/install -r requirements.txt

FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PATH=/install/bin:$PATH \
    PYTHONPATH=/install/lib/python3.12/site-packages:/srv/ai
WORKDIR /srv/ai
COPY --from=builder /install /install
COPY app ./app
COPY migrations ./migrations
COPY prompts ./prompts

RUN groupadd -r app && useradd -r -g app app \
    && chown -R app:app /srv/ai
USER app
EXPOSE 8000

# liveness: the FastAPI /healthz (stdlib urllib — no curl in slim).
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD python -c "import sys,urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/healthz', timeout=2).status == 200 else 1)"

ENTRYPOINT ["python", "-m", "app.main"]
