# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.MixProject do
  use Mix.Project

  def project do
    [
      app: :sutra_chat,
      version: "0.1.0",
      elixir: "~> 1.14",
      start_permanent: Mix.env() == :prod,
      deps: deps()
    ]
  end

  def application do
    [
      mod: {SutraChat.Application, []},
      # :inets + :ssl back the JWKS fetches in TokenVerifier (stdlib HTTP,
      # no extra dependency). :crypto backs libcluster gossip HMAC and the
      # per-device presence session ids.
      extra_applications: [:logger, :runtime_tools, :inets, :ssl, :crypto]
    ]
  end

  defp deps do
    [
      {:phoenix, "~> 1.7.0"},
      {:phoenix_pubsub, "~> 2.1"},
      {:jason, "~> 1.4"},
      {:bandit, "~> 1.5"},
      {:ecto_sql, "~> 3.11"},
      {:postgrex, "~> 0.19"},
      # Multi-node clustering for Phoenix.Presence / Phoenix.PubSub CRDT
      # convergence (B5). Gossip strategy for dev/local, Kubernetes
      # strategy (commented config) for k8s.
      {:libcluster, "~> 3.3"}
    ]
  end
end
