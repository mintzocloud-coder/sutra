# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.UserChannelTest do
  @moduledoc """
  Tests for the D5 per-user event channel.

  Join auth is the whole point of this channel: a socket may join only
  its OWN `user:<subject>` topic — users get only their own events, and
  a user can never subscribe to someone else's. The event-shape tests
  prove a broadcast on `user:alice` reaches alice's subscriber with the
  documented payload intact, and never leaks onto another user's topic.
  """
  use SutraChatWeb.ChannelCase, async: false

  defp socket_for(user_id) do
    socket(SutraChatWeb.UserSocket, "user_socket:#{user_id}", %{user_id: user_id})
  end

  # -- join auth --------------------------------------------------------

  test "join succeeds on the socket's own topic" do
    assert {:ok, _, _} =
             subscribe_and_join(socket_for("alice"), SutraChatWeb.UserChannel, "user:alice")
  end

  test "join refuses another user's topic" do
    assert {:error, %{reason: "unauthorized"}} =
             subscribe_and_join(socket_for("alice"), SutraChatWeb.UserChannel, "user:bob")
  end

  test "join refuses a socket with no authenticated subject" do
    anon = socket(SutraChatWeb.UserSocket, "user_socket:anon", %{})

    assert {:error, %{reason: "unauthorized"}} =
             subscribe_and_join(anon, SutraChatWeb.UserChannel, "user:alice")
  end

  test "join refuses malformed topics" do
    assert {:error, %{reason: "invalid topic"}} =
             subscribe_and_join(socket_for("alice"), SutraChatWeb.UserChannel, "user:")

    assert {:error, %{reason: "invalid topic"}} =
             subscribe_and_join(socket_for("alice"), SutraChatWeb.UserChannel, "user:a:b")
  end

  # -- event shape + topic isolation ------------------------------------

  test "post:new on the user's topic reaches that user with the documented shape" do
    {:ok, _, _} =
      subscribe_and_join(socket_for("alice"), SutraChatWeb.UserChannel, "user:alice")

    {:ok, _, _} =
      subscribe_and_join(socket_for("bob"), SutraChatWeb.UserChannel, "user:bob")

    payload = %{
      "post_id" => "post-1",
      "author_id" => "bob-id",
      "author_handle" => "bob",
      "text" => "hello alice",
      "media_ids" => [],
      "created_at" => "2026-10-18T10:00:00Z"
    }

    :ok = SutraChatWeb.Endpoint.broadcast("user:alice", "post:new", payload)

    # subscribe_and_join subscribed this test process to both topics, so
    # the topic on the broadcast struct is what proves isolation: alice's
    # topic got it, bob's never sees it.
    assert_receive %Phoenix.Socket.Broadcast{
      topic: "user:alice",
      event: "post:new",
      payload: ^payload
    }

    refute_receive %Phoenix.Socket.Broadcast{topic: "user:bob", event: "post:new"}
  end

  test "follower:new on the user's topic carries the documented payload" do
    {:ok, _, _} =
      subscribe_and_join(socket_for("bob"), SutraChatWeb.UserChannel, "user:bob")

    payload = %{
      "follower_id" => "follower-uuid",
      "follower_handle" => "alice",
      "created_at" => "2026-10-18T10:00:00Z"
    }

    :ok = SutraChatWeb.Endpoint.broadcast("user:bob", "follower:new", payload)

    assert_receive %Phoenix.Socket.Broadcast{
      topic: "user:bob",
      event: "follower:new",
      payload: ^payload
    }
  end

  # -- e2e:prekey_fetch (I1) -------------------------------------------------

  setup do
    SutraChat.DataCase.create_e2e_tables!()
    :ok
  end

  test "e2e:prekey_fetch returns the bundle and consumes one prekey per call" do
    SutraChat.DataCase.insert_e2e_device!("bob", "pixel-9", [1, 2])
    {:ok, _, socket} = subscribe_and_join(socket_for("alice"), SutraChatWeb.UserChannel, "user:alice")

    # NOTE: Phoenix.ChannelTest.push/3 returns a reference, never the
    # reply tuple — the reply is asserted with assert_reply (the B6
    # chat-channel convention). An earlier draft of these tests matched
    # {:ok, _} directly on push/3 and failed with a bare #Reference.
    ref1 = push(socket, "e2e:prekey_fetch", %{"user_id" => "bob", "device_id" => "pixel-9"})
    assert_reply ref1, :ok, %{
      "device_id" => "pixel-9",
      "one_time_prekey" => %{"id" => 1},
      "available_prekeys" => 1
    }

    ref2 = push(socket, "e2e:prekey_fetch", %{"user_id" => "bob", "device_id" => "pixel-9"})
    assert_reply ref2, :ok, %{"one_time_prekey" => %{"id" => 2}, "available_prekeys" => 0}

    ref3 = push(socket, "e2e:prekey_fetch", %{"user_id" => "bob", "device_id" => "pixel-9"})
    assert_reply ref3, :ok, %{"one_time_prekey" => nil}
  end

  test "e2e:prekey_fetch on an unknown device replies not_found" do
    {:ok, _, socket} = subscribe_and_join(socket_for("alice"), SutraChatWeb.UserChannel, "user:alice")

    ref = push(socket, "e2e:prekey_fetch", %{"user_id" => "nobody", "device_id" => "x"})
    assert_reply ref, :error, %{reason: "not_found"}
  end

  test "e2e:prekey_fetch with a malformed payload replies invalid_request" do
    {:ok, _, socket} = subscribe_and_join(socket_for("alice"), SutraChatWeb.UserChannel, "user:alice")

    ref1 = push(socket, "e2e:prekey_fetch", %{"user_id" => "bob"})
    assert_reply ref1, :error, %{reason: "invalid_request"}

    ref2 = push(socket, "e2e:prekey_fetch", %{"user_id" => "bob", "device_id" => ""})
    assert_reply ref2, :error, %{reason: "invalid_request"}
  end
end
