# SPDX-License-Identifier: AGPL-3.0-or-later
import Config

config :sutra_chat, SutraChatWeb.Endpoint,
  url: [host: "localhost"],
  adapter: Bandit.PhoenixAdapter,
  http: [ip: {0, 0, 0, 0}, port: 4000],
  server: true,
  secret_key_base: "dev-only-secret-key-base-replace-in-prod-0123456789abcdef",
  # PubSub the endpoint broadcasts/subscribes through. Started as
  # {Phoenix.PubSub, name: SutraChat.PubSub} in the application
  # supervision tree; Phoenix.Presence uses the same server. Without
  # this, Endpoint.broadcast/3 + Endpoint.subscribe/2 raise
  # "no :pubsub_server configured" — which broke both the
  # D5 /internal/events fan-out and every channel test that subscribes
  # via the endpoint (CI red 2026-10-18).
  pubsub_server: SutraChat.PubSub

# Lets `mix ecto.*` find the repo. Connection details: dev defaults here,
# prod comes from DATABASE_URL in config/runtime.exs.
config :sutra_chat,
  ecto_repos: [SutraChat.Repo]

config :sutra_chat, SutraChat.Repo,
  database: "sutra_chat_dev",
  username: "postgres",
  password: "postgres",
  hostname: "localhost",
  pool_size: 10,
  # The Go API service runs its own migration ledger on the shared
  # database (`schema_migrations`, integer versions). Ecto's default
  # ledger table has the same name but a different shape, so the chat
  # service keeps its versions in a separate table — otherwise whichever
  # service migrates second would corrupt the first's ledger.
  migration_source: "chat_schema_migrations"

# Shared identity contract with the Go API service: websocket tokens are
# verified against the same OIDC issuer's JWKS, configured with the same
# env vars the API uses (HYDRA_ISSUER / JWT_AUDIENCE / AUTH_BYPASS /
# AUTH_BYPASS_SUBJECT). See docs/auth-contract.md.
config :sutra_chat, :auth,
  issuer: System.get_env("HYDRA_ISSUER") || "http://hydra:4444",
  audience: System.get_env("JWT_AUDIENCE") || "sutra-api",
  bypass: System.get_env("AUTH_BYPASS") == "1",
  bypass_subject: System.get_env("AUTH_BYPASS_SUBJECT") || "dev-user"

config :logger, :console,
  format: "$time $metadata[$level] $message\n",
  metadata: [:request_id]

# Per-environment overrides (config/test.exs carries the sandbox Repo,
# server: false endpoint, and quiet logger for `mix test`).
import_config "#{config_env()}.exs"
