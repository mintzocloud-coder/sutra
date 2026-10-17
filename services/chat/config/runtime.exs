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

  # Same identity contract as the Go API service, read at boot (not build)
  # so operators configure identity once for both services.
  config :sutra_chat, :auth,
    issuer: System.get_env("HYDRA_ISSUER") || "http://hydra:4444",
    audience: System.get_env("JWT_AUDIENCE") || "sutra-api",
    bypass: System.get_env("AUTH_BYPASS") == "1",
    bypass_subject: System.get_env("AUTH_BYPASS_SUBJECT") || "dev-user"
end
