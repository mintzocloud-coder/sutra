# SPDX-License-Identifier: AGPL-3.0-or-later
# Sutra API — production image.
#   - Multi-stage: compile in the Go toolchain image, ship only the static binary.
#   - Minimal base (alpine), non-root runtime user, DB-independent liveness probe.
# The Go binary is the container's PID 1 (exec-form ENTRYPOINT), so it receives
# SIGTERM directly and the in-process graceful shutdown (signal.Notify +
# http.Server.Shutdown in cmd/api) drains in-flight requests on orchestrator stops.

# ---- build stage -----------------------------------------------------------
FROM golang:1.22-alpine AS build
WORKDIR /src

# Copy module metadata first so `go mod download` stays cached unless deps change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary (CGO off → no libc dependency), reproducible (-trimpath),
# stripped (-s -w) to keep the shipped image small.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /api ./cmd/api

# ---- runtime stage ---------------------------------------------------------
FROM alpine:3.20

# Dedicated unprivileged account: the API process must never run as root, so a
# container escape would land on an account with nothing to do.
RUN addgroup -S app && adduser -S -G app app
COPY --from=build --chown=app:app /api /api
USER app

EXPOSE 8080

# Liveness only: /healthz never touches the database, so a database outage does
# not make the orchestrator restart a healthy API process. Readiness (whether
# the process can serve traffic) is the DB-aware /readyz endpoint and is wired
# separately in compose/k8s, not here.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/api"]
