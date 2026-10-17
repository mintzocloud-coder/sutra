# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Application do
  @moduledoc false
  use Application

  @impl true
  def start(_type, _args) do
    children = [
      SutraChat.Repo,
      {Phoenix.PubSub, name: SutraChat.PubSub},
      SutraChat.Presence,
      SutraChatWeb.Endpoint
    ]

    opts = [strategy: :one_for_one, name: SutraChat.Supervisor]
    Supervisor.start_link(children, opts)
  end
end
