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
      extra_applications: [:logger, :runtime_tools]
    ]
  end

  defp deps do
    [
      {:phoenix, "~> 1.7.0"},
      {:phoenix_pubsub, "~> 2.1"},
      {:jason, "~> 1.4"},
      {:bandit, "~> 1.5"}
    ]
  end
end
