# SPDX-License-Identifier: AGPL-3.0-or-later
# Production image for the Sutra chat service.
#
# B7: hardened — built with `mix release` (never `mix phx.server` /
# `mix run` in production), non-root runtime user, minimal Alpine runtime
# stage with the ERTS bundled into the release. `mix phx.server` stays the
# dev workflow (see README.md).
#
# Build:  docker build -f services/chat/Dockerfile services/chat
# Run:    docker run -e DATABASE_URL=... -e SECRET_KEY_BASE=... -p 4000:4000 <image>
# Migrate (once per deploy, before taking traffic):
#         docker run --rm -e DATABASE_URL=... <image> \
#           /app/bin/sutra_chat eval "SutraChat.Release.migrate()"

FROM elixir:1.18-alpine AS build

RUN apk add --no-cache build-base git
RUN mix local.hex --force && mix local.rebar --force

WORKDIR /app
ENV MIX_ENV=prod

# Deps first for better layer caching; app code after.
COPY mix.exs mix.lock ./
RUN mix deps.get --only prod
RUN mix deps.compile

COPY config config/
COPY priv priv/
COPY lib lib/
RUN mix compile
RUN mix release

# Builder is elixir:1.18-alpine (OTP 27, Alpine 3.21-based): keep the runtime
# on the same Alpine generation so musl/libstdc++ stay compatible with the
# bundled ERTS.
FROM alpine:3.21 AS runtime

# Runtime needs only: C/C++ stdlibs (ERTS + Bandit NIFs), ncurses (ERTS),
# CA certificates (JWKS fetches over HTTPS — see docs/auth-contract.md).
# No build tools, no source, no mix/hex.
RUN apk add --no-cache libstdc++ libgcc ncurses-libs ca-certificates \
  && addgroup -S app \
  && adduser -S -G app app

WORKDIR /app
COPY --from=build --chown=app:app /app/_build/prod/rel/sutra_chat ./

USER app
EXPOSE 4000
ENV HOME=/app MIX_ENV=prod

# The endpoint is websocket-only (no HTTP routes yet), so liveness is the
# release's own pid check rather than an HTTP probe. If an HTTP /healthz
# lands later, prefer it here.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/app/bin/sutra_chat", "pid"]

# Foreground start: the BEAM is PID 1, so SIGTERM reaches it directly and
# the app's graceful shutdown runs (mirrors the Go API's A8 behavior).
CMD ["/app/bin/sutra_chat", "start"]
