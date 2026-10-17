# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Presence do
  @moduledoc """
  CRDT-based presence tracking (`Phoenix.Presence` / `Phoenix.Tracker`).

  ## Model

  * **Key** is the authenticated subject (`socket.assigns.user_id`) — one
    presence entry per user per room topic, exactly like the `sender_subject`
    identity used for messages.
  * **One meta per tracking process.** Each channel join calls
    `Presence.track/4`, and the tracker keeps one meta per tracking pid, so
    the same user on phone + laptop accumulates two metas under their key
    (multi-device-ready, matching the Wave-2 multi-device session model).
  * **Meta shape** (set by `ChatChannel.handle_info(:after_join, ...)`):
    `%{online_at: <unix ms>, device: <device_id>, status: "online"}`.
    `device_id` is client-supplied on join (stable per app install, so
    clients can label "phone" vs "laptop"); when absent the server mints
    an `anon-<random>` id for that session.

  ## Events clients consume

  * `"presence_state"` — pushed once per channel join with the full
    `Presence.list/1` map. Clients render this as the initial roster.
  * `"presence_diff"` — broadcast automatically by `Phoenix.Presence` on
    every track/untrack, carrying `%{joins: %{key => %{metas: [...]}},
    leaves: %{key => %{metas: [...]}}}`. Clients apply diffs to the roster;
    diffs are the ONLY incremental update path (there is no polling).

  ## Convergence

  The tracker is a delta-based CRDT (`Phoenix.Tracker` over
  `Phoenix.PubSub`): joins and leaves converge on all nodes even across
  netsplits, with last-writer-wins per replica clock. Consequences:

  * A node crash leaves "ghost" entries only until the tracker's
    heartbeat detects the dead replica — no manual cleanup needed.
  * `presence_diff` payloads may contain both a join AND a leave for the
    same key (a device reconnecting); clients must apply joins before
    leaves within one diff.
  * Presence is best-effort liveness, NOT delivery state: a user can be
    "present" and still miss a broadcast during a netsplit. Delivery
    guarantees live in `docs/delivery-semantics.md`, not here.

  ## Multi-node

  Presence converges across chat nodes only when the BEAM nodes are
  connected — see `config/runtime.exs` (`CLUSTER_ENABLED=1`, gossip for
  dev/local, Kubernetes DNS strategy commented for k8s) and
  `docs/presence.md`. Single-node deployments need no clustering.
  """
  use Phoenix.Presence,
    otp_app: :sutra_chat,
    pubsub_server: SutraChat.PubSub
end
