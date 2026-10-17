# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Release do
  @moduledoc """
  Release-time tasks for the `mix release` production image (B7).

  A release has no `mix` available, so anything `mix ecto.*` does in dev
  needs an equivalent here, invoked through the release script:

      bin/sutra_chat eval "SutraChat.Release.migrate()"

  (Run this once per deploy before the new image takes traffic; migrations
  are forward-only and advisory-locked like the Go API's — see
  services/api/internal/db.)
  """
  @app :sutra_chat

  @doc "Apply all pending migrations (priv/repo/migrations)."
  def migrate do
    load_app()

    for repo <- repos() do
      {:ok, _, _} = Ecto.Migrator.with_repo(repo, &Ecto.Migrator.run(&1, :up, all: true))
    end
  end

  @doc "Roll a repo back to a specific migration version."
  def rollback(repo, version) do
    load_app()
    {:ok, _, _} = Ecto.Migrator.with_repo(repo, &Ecto.Migrator.run(&1, :down, to: version))
  end

  defp repos do
    Application.fetch_env!(@app, :ecto_repos)
  end

  defp load_app do
    # `with_repo` starts the repo's dependencies (postgrex) on demand
    # without booting the whole application.
    Application.load(@app)
  end
end
