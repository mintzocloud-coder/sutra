# Presence

How "who's online" works in Sutra chat — the model, the events, and the
multi-node story.

## Model

* **Key = authenticated subject.** Each room topic (`room:<id>`) tracks
  users under their OIDC subject — the same identity used as
  `sender_subject` on messages. No anonymous presence: sockets are
  refused without a valid token (`UserSocket.connect/3`), so every
  presence entry is attributable.
* **One meta per device session.** `Phoenix.Presence` keeps one meta per
  tracking process, and the channel tracks once per join. The same user
  on phone + laptop therefore shows **two metas under one key**:
  `%{online_at: <unix ms>, device: <device_id>, status: "online"}`.
  Clients send a stable `device_id` join param (per app install) so the
  roster can label devices; when absent the server mints an
  `anon-<random>` id for that session.
* **Wave 2 note:** when membership lists land (`docs/room-membership.md`),
  presence keys stay subject-based; only the join authorization changes.

## Events (client contract)

| Event | When | Payload |
|---|---|---|
| `presence_state` | pushed once per channel join | full `Presence.list/1` map: `%{user_id => %{metas: [...]}}` |
| `presence_diff` | broadcast on every track/untrack | `%{joins: %{user_id => %{metas: [...]}}, leaves: %{...}}` |

`presence_diff` is emitted by `Phoenix.Presence` itself — the channel
never fabricates it. Client rules:

1. Render `presence_state` as the initial roster.
2. Apply diffs incrementally — **never poll**.
3. Within one diff, apply `joins` before `leaves` (a reconnecting device
   can appear in both).
4. A user with zero remaining metas is offline; with ≥1 meta, online on
   the listed devices.

## Convergence (CRDT)

Presence rides `Phoenix.Tracker`, a delta-based CRDT over
`Phoenix.PubSub`:

* Joins/leaves converge on every connected node, including across
  netsplits (last-writer-wins per replica clock).
* A crashed node leaves ghost entries only until the heartbeat notices
  the dead replica — no manual cleanup, no TTL tuning.
* Presence is **liveness, not delivery state**: a "present" user can
  still miss a broadcast during a netsplit. Delivery guarantees are
  documented separately in `docs/delivery-semantics.md`.

## Multi-node clustering

Presence converges across chat nodes **only if the BEAM nodes are
connected**. Wiring lives in `services/chat/config/runtime.exs` and is
opt-in via `CLUSTER_ENABLED=1` (single-node deployments need nothing).

* **Local/dev** — gossip strategy (UDP multicast `230.1.1.251:45892`).
  Run two named nodes and watch presence converge:
  `CLUSTER_ENABLED=1 iex --sname chat1 -S mix phx.server`
  `CLUSTER_ENABLED=1 iex --sname chat2 -S mix phx.server`
* **Kubernetes** — a commented DNS-discovery topology
  (`Cluster.Strategy.Kubernetes`) sits next to the gossip config; enable
  it instead of gossip in k8s (multicast rarely survives CNI).
* **Cookie** — all nodes MUST share the Erlang cookie or `Node.connect`
  is refused. Releases take it from the standard `RELEASE_COOKIE` env
  var (with `RELEASE_NODE` for the node name).
* **Secret** — gossip payloads are HMAC-signed with `CLUSTER_SECRET`;
  set a real value in prod so a rogue host on the multicast group can't
  inject fake node joins. Dev default is intentionally weak and logged
  as such by the strategy.

`Phoenix.PubSub` 2.x distributes over the same node connections, so one
clustering setup covers both presence and broadcast fan-out.
