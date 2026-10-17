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
      # libcluster topology workers (B5): with no topologies configured
      # (default — clustering is opt-in via CLUSTER_ENABLED=1) this is a
      # no-op supervisor. When enabled, it connects BEAM nodes so
      # Phoenix.Presence / PubSub CRDTs converge across chat nodes.
      {Cluster.Supervisor,
       [Application.get_env(:libcluster, :topologies, []), [name: SutraChat.ClusterSupervisor]]},
      SutraChatWeb.Endpoint
    ]

    opts = [strategy: :one_for_one, name: SutraChat.Supervisor]
    Supervisor.start_link(children, opts)
  end
end
