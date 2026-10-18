# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.PresenceTest do
  @moduledoc """
  Single-node presence behavior (B5): track/list/untrack, multi-device
  metas under one key, and the automatic `presence_diff` broadcast.

  These run against the real `SutraChat.Presence` (Phoenix.Presence CRDT)
  on a single node — they prove the local contract. Cross-node
  convergence is exercised manually via CLUSTER_ENABLED=1 (see
  docs/presence.md); there is no multi-node CI yet.
  """
  use ExUnit.Case, async: false

  alias SutraChat.Presence

  @pubsub SutraChat.PubSub

  setup do
    # `mix test` boots the application (PubSub + Presence included), so in
    # CI this branch is a no-op. The fallback keeps the test runnable with
    # --no-start: Presence only exposes child_spec/1 (no start_link/0),
    # so both children are started through a throwaway supervisor.
    # NOTE: the probe must be SutraChat.Presence — the registered name of
    # the tracker process itself (started directly in the application
    # tree). A stale version probed SutraChat.PresenceSupervisor, which
    # does not exist, so the fallback started a SECOND PubSub under the
    # same name and every test died with "already started" (CI red
    # 2026-10-18).
    unless Process.whereis(SutraChat.Presence) do
      {:ok, _} =
        Supervisor.start_link([{Phoenix.PubSub, name: @pubsub}, SutraChat.Presence],
          strategy: :one_for_one
        )
    end

    topic = "room:test-#{System.unique_integer([:positive])}"
    :ok = Phoenix.PubSub.subscribe(@pubsub, topic)
    {:ok, topic: topic}
  end

  test "track makes the user visible in list/1 with the channel meta shape", %{topic: topic} do
    meta = %{online_at: System.system_time(:millisecond), device: "phone-1", status: "online"}
    assert {:ok, _ref} = Presence.track(self(), topic, "user-1", meta)

    assert %{"user-1" => %{metas: [listed]}} = Presence.list(topic)
    assert listed.device == "phone-1"
    assert listed.status == "online"
    assert is_integer(listed.online_at)
  end

  test "the same user on two devices accumulates two metas under one key", %{topic: topic} do
    holder = fn device, parent ->
      {:ok, _} = Presence.track(self(), topic, "user-1", %{device: device, status: "online"})
      send(parent, {:tracked, device})

      receive do
        :stop -> :ok
      end
    end

    # NOTE: capture the test pid BEFORE spawning — `self()` inside the
    # spawned function is the child's pid, so passing `self()` there sends
    # the :tracked confirmation to the child itself (which is blocked in
    # `receive :stop`) and the test times out waiting for it (CI red
    # 2026-10-19). The tracked pid (Presence.track's first arg) stays the
    # child's — two children = two metas under one key, which is the point.
    test_pid = self()
    p1 = spawn(fn -> holder.("phone", test_pid) end)
    p2 = spawn(fn -> holder.("laptop", test_pid) end)
    assert_receive {:tracked, "phone"}, 5_000
    assert_receive {:tracked, "laptop"}, 5_000

    assert %{"user-1" => %{metas: metas}} = Presence.list(topic)
    assert Enum.sort(Enum.map(metas, & &1.device)) == ["laptop", "phone"]

    send(p1, :stop)
    send(p2, :stop)
  end

  test "untrack removes the key and broadcasts a leaves diff", %{topic: topic} do
    {:ok, _} = Presence.track(self(), topic, "user-1", %{device: "phone", status: "online"})
    assert_receive %Phoenix.Socket.Broadcast{event: "presence_diff", payload: %{joins: joins}}, 5_000
    assert Map.has_key?(joins, "user-1")

    Presence.untrack(self(), topic, "user-1")

    assert_receive %Phoenix.Socket.Broadcast{
                     event: "presence_diff",
                     payload: %{leaves: %{"user-1" => %{metas: [_]}}}
                   },
                   5_000

    assert Presence.list(topic) == %{}
  end

  test "track broadcasts a joins diff to topic subscribers", %{topic: topic} do
    {:ok, _} = Presence.track(self(), topic, "user-2", %{device: "web", status: "online"})

    assert_receive %Phoenix.Socket.Broadcast{
                     topic: ^topic,
                     event: "presence_diff",
                     payload: %{joins: %{"user-2" => %{metas: [%{device: "web"}]}}, leaves: %{}}
                   },
                   5_000
  end

  test "a dead tracking process is untracked automatically", %{topic: topic} do
    pid =
      spawn(fn ->
        {:ok, _} = Presence.track(self(), topic, "user-3", %{device: "phone", status: "online"})
        # Never exits cleanly — dies when the test kills it.
        Process.sleep(:infinity)
      end)

    assert_receive %Phoenix.Socket.Broadcast{event: "presence_diff", payload: %{joins: joins}},
                   5_000

    assert Map.has_key?(joins, "user-3")
    Process.exit(pid, :kill)

    assert_receive %Phoenix.Socket.Broadcast{event: "presence_diff", payload: %{leaves: leaves}},
                   5_000

    assert Map.has_key?(leaves, "user-3")
    assert Presence.list(topic) == %{}
  end
end
