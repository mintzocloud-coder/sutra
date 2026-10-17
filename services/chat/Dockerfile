# SPDX-License-Identifier: AGPL-3.0-or-later
FROM elixir:1.18-alpine AS build
RUN apk add --no-cache build-base git
RUN mix local.hex --force && mix local.rebar --force
WORKDIR /app
ENV MIX_ENV=prod
COPY mix.exs ./
RUN mix deps.get --only prod
COPY . .
RUN mix compile

FROM elixir:1.18-alpine
RUN apk add --no-cache libstdc++ ncurses-libs
WORKDIR /app
ENV MIX_ENV=prod
COPY --from=build /app ./
COPY --from=build /root/.mix /root/.mix
EXPOSE 4000
CMD ["mix", "run", "--no-halt"]
