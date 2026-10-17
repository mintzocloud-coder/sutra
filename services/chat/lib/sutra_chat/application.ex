# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Application do
  @moduledoc false
  use Application

  @impl true
  def start(_type, _args) do
    children = [
      SutraChat.Repo,
      # JWKS cache for websocket auth — before the Endpoint so sockets
      # never accept connects while the verifier is down.
      SutraChat.Auth.TokenVerifier,
      {Phoenix.PubSub, name: SutraChat.PubSub},
      SutraChat.Presence,
      SutraChatWeb.Endpoint
    ]

    opts = [strategy: :one_for_one, name: SutraChat.Supervisor]
    Supervisor.start_link(children, opts)
  end
end
