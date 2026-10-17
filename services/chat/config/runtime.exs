# SPDX-License-Identifier: AGPL-3.0-or-later
import Config

# Runs before boot in prod (compose, releases). Lets DATABASE_URL decide the
# connection; the API service uses the same variable, so one compose file
# wires both services to the same postgres.
if config_env() == :prod do
  database_url =
    System.get_env("DATABASE_URL") ||
      raise "DATABASE_URL is not set (required in prod for SutraChat.Repo)"

  config :sutra_chat, SutraChat.Repo,
    url: database_url,
    pool_size: String.to_integer(System.get_env("POOL_SIZE") || "10")
end
