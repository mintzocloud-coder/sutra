# SPDX-License-Identifier: AGPL-3.0-or-later
import Config

# Channel tests (B6) run against a real PostgreSQL through the Ecto SQL
# sandbox: each test checks out a connection and rolls back afterwards,
# so tests stay isolated without truncating tables. `mix test` boots the
# application (Repo + PubSub + Presence + Endpoint); `server: false` keeps
# the HTTP listener down — channel tests join in-process via
# Phoenix.ChannelTest and never touch the network.
config :sutra_chat, SutraChatWeb.Endpoint,
  http: [ip: {127, 0, 0, 1}, port: 4002],
  secret_key_base: "test-only-secret-key-base-not-for-production-use-0123456789abcdef",
  server: false

# CI sets DATABASE_URL to its postgres service; local runs fall back to a
# dedicated test database on localhost (never the dev database).
if database_url = System.get_env("DATABASE_URL") do
  config :sutra_chat, SutraChat.Repo,
    url: database_url,
    pool: Ecto.Adapters.SQL.Sandbox,
    pool_size: System.schedulers_online() * 2
else
  config :sutra_chat, SutraChat.Repo,
    database: "sutra_chat_test",
    username: "postgres",
    password: "postgres",
    hostname: "localhost",
    pool: Ecto.Adapters.SQL.Sandbox,
    pool_size: System.schedulers_online() * 2
end

# Keep test output readable: warnings and errors only.
config :logger, level: :warning
