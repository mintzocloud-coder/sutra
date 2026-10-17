# SPDX-License-Identifier: AGPL-3.0-or-later
import Config

config :sutra_chat, SutraChatWeb.Endpoint,
  url: [host: "localhost"],
  adapter: Bandit.PhoenixAdapter,
  http: [ip: {0, 0, 0, 0}, port: 4000],
  server: true,
  secret_key_base: "dev-only-secret-key-base-replace-in-prod-0123456789abcdef"

# Lets `mix ecto.*` find the repo. Connection details: dev defaults here,
# prod comes from DATABASE_URL in config/runtime.exs.
config :sutra_chat,
  ecto_repos: [SutraChat.Repo]

config :sutra_chat, SutraChat.Repo,
  database: "sutra_chat_dev",
  username: "postgres",
  password: "postgres",
  hostname: "localhost",
  pool_size: 10

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
