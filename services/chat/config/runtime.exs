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

# -- Multi-node clustering (B5) ---------------------------------------
# Phoenix.Presence / Phoenix.PubSub are CRDTs: presence converges across
# chat nodes ONLY when the BEAM nodes are connected. Clustering is opt-in
# via CLUSTER_ENABLED=1 so a plain single-node `mix phx.server` stays
# untouched. When enabled, libcluster's gossip strategy discovers peers
# over UDP multicast — enough to run two local nodes and watch presence
# converge:
#
#   CLUSTER_ENABLED=1 iex --sname chat1 -S mix phx.server
#   CLUSTER_ENABLED=1 iex --sname chat2 -S mix phx.server
#
# (Distribution requires named nodes; releases — see B7 — enable it via
# the standard RELEASE_NODE / RELEASE_COOKIE env vars. ALL nodes in a
# cluster MUST share the same cookie, otherwise Node.connect is refused.)
#
# Gossip is fine for local/dev. In Kubernetes prefer DNS discovery over
# multicast — the commented topology below is the k8s-ready alternative.
topologies =
  if System.get_env("CLUSTER_ENABLED") == "1" do
    [
      sutra_gossip: [
        strategy: Cluster.Strategy.Gossip,
        config: [
          port: 45892,
          if_addr: "0.0.0.0",
          multicast_addr: "230.1.1.251",
          multicast_ttl: 1,
          # Signs gossip payloads; set a real secret in prod so a rogue
          # pod on the multicast group can't inject fake node joins.
          secret: System.get_env("CLUSTER_SECRET") || "dev-only-cluster-secret"
        ]
      ]
      # Kubernetes (commented example — enable instead of gossip in k8s):
      # sutra_k8s: [
      #   strategy: Cluster.Strategy.Kubernetes,
      #   config: [
      #     mode: :dns,
      #     kubernetes_node_basename: "sutra-chat",
      #     kubernetes_selector: "app=sutra-chat",
      #     kubernetes_namespace: "sutra",
      #     polling_interval: 10_000
      #   ]
      # ]
    ]
  else
    []
  end

config :libcluster, topologies: topologies
