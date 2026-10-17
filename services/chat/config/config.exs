# SPDX-License-Identifier: AGPL-3.0-or-later
import Config

config :sutra_chat, SutraChatWeb.Endpoint,
  url: [host: "localhost"],
  adapter: Bandit.PhoenixAdapter,
  http: [ip: {0, 0, 0, 0}, port: 4000],
  server: true,
  secret_key_base: "dev-only-secret-key-base-replace-in-prod-0123456789abcdef"

config :logger, :console,
  format: "$time $metadata[$level] $message\n",
  metadata: [:request_id]
