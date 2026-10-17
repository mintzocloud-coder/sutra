# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.ChatChannelTest do
  @moduledoc """
  Channel integration tests (B6): join/auth, send/persist, idempotent
  dedupe, and cursor-paged history.

  These run against the real `SutraChat.Repo` (PostgreSQL via the Ecto SQL
  sandbox — every test rolls back) and the real `SutraChat.Presence`, with
  sockets joined in-process through `Phoenix.ChannelTest`.

  Socket-level auth (`UserSocket.connect/3` token verification) is covered
  separately in `SutraChat.Auth.TokenTest`; here the socket assigns are
  built directly, which also proves the channel's own defense-in-depth:
  a socket that somehow arrives *without* an identity is refused at join.
  """
  use SutraChatWeb.ChannelCase, async: false

  alias SutraChat.Chat

  @user "channel-test-user"

  setup do
    socket = socket(SutraChatWeb.UserSocket, "user_socket:#{@user}", %{user_id: @user})
    {:ok, socket: socket}
  end

  # -- join / auth --------------------------------------------------------

  test "join refuses a socket with no authenticated subject", %{socket: _socket} do
    anon = socket(SutraChatWeb.UserSocket, "user_socket:anon", %{})

    assert {:error, %{reason: "unauthorized"}} =
             subscribe_and_join(anon, SutraChatWeb.ChatChannel, "room:general")
  end

  test "join refuses malformed room ids", %{socket: socket} do
    assert {:error, %{reason: "invalid room id"}} =
             subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:has space!")

    assert {:error, %{reason: "invalid room id"}} =
             subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:../escape")
  end

  test "join succeeds and returns an empty history for a fresh room", %{socket: socket} do
    assert {:ok, %{room_id: "room:fresh-" <> _, history: %{messages: [], next_cursor: nil}}, _} =
             subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:fresh-#{uniq()}")
  end

  test "join tracks presence and pushes the roster", %{socket: socket} do
    {:ok, _, _} =
      subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:presence-#{uniq()}")

    # Full roster pushed to the joining socket...
    assert_push "presence_state", %{@user => %{metas: [meta]}}
    assert meta.status == "online"
    assert is_binary(meta.device)
    assert is_integer(meta.online_at)

    # ...and a joins diff broadcast to existing subscribers.
    assert_broadcast "presence_diff", %{joins: %{@user => %{metas: [_]}}}
  end

  test "join accepts limit and cursor params, rejects bad cursors", %{socket: socket} do
    topic = "room:join-params-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, topic)

    for i <- 1..3 do
      push(socket, "message:new", %{"body" => "m#{i}", "client_msg_id" => "jp-#{i}"})
      assert_push "msg:ack", %{client_msg_id: "jp-" <> _}
    end

    socket2 = socket(SutraChatWeb.UserSocket, "user_socket:#{@user}", %{user_id: @user})

    assert {:ok, %{history: %{messages: messages, next_cursor: cursor}}, _} =
             subscribe_and_join(socket2, SutraChatWeb.ChatChannel, topic, %{"limit" => "2"})

    assert length(messages) == 2
    assert is_binary(cursor)

    socket3 = socket(SutraChatWeb.UserSocket, "user_socket:#{@user}", %{user_id: @user})

    assert {:error, %{reason: "invalid history cursor"}} =
             subscribe_and_join(socket3, SutraChatWeb.ChatChannel, topic, %{
               "cursor" => "not-a-cursor"
             })
  end

  # -- send / persist -----------------------------------------------------

  test "send persists, broadcasts, and acks only the sender", %{socket: socket} do
    topic = "room:send-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, topic)

    push(socket, "message:new", %{"body" => "hello sutra", "client_msg_id" => "send-1"})

    # Every subscriber (including this test, which subscribed on join)
    # sees the broadcast...
    assert_broadcast "message:new", broadcast
    assert broadcast.body == "hello sutra"
    assert broadcast.client_msg_id == "send-1"
    assert broadcast.room_id == String.replace_prefix(topic, "room:", "")
    assert broadcast.user_id == @user
    assert is_binary(broadcast.id)
    assert is_integer(broadcast.sent_at)

    # ...and the sender — only the sender — gets exactly one ack, a pushed
    # event that can never precede durability.
    assert_push "msg:ack", ack
    assert ack.deduped == false
    assert ack.client_msg_id == "send-1"
    assert ack.id == broadcast.id
    assert is_integer(ack.sent_at)

    # The row is really in PostgreSQL: a fresh join sees it in history.
    assert {:ok, %{messages: [stored]}} = Chat.history(String.replace_prefix(topic, "room:", ""))
    assert stored.body == "hello sutra"
    assert stored.client_msg_id == "send-1"
    assert stored.sender_subject == @user
  end

  test "retried send with the same client_msg_id is deduped", %{socket: socket} do
    room = "dedupe-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    push(socket, "message:new", %{"body" => "once", "client_msg_id" => "dup-1"})
    assert_push "msg:ack", %{deduped: false, id: first_id}

    # Same client_msg_id again: no second row, ack carries deduped: true
    # with the ORIGINAL server id — the client learns "already had it".
    push(socket, "message:new", %{"body" => "once", "client_msg_id" => "dup-1"})
    assert_push "msg:ack", %{deduped: true, id: ^first_id}

    assert {:ok, %{messages: messages}} = Chat.history(room)
    assert length(messages) == 1
    assert hd(messages).id == first_id
  end

  test "history survives across joins, oldest-first", %{socket: socket} do
    room = "rejoin-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    for {body, i} <- Enum.with_index(["one", "two", "three"]) do
      push(socket, "message:new", %{"body" => body, "client_msg_id" => "rh-#{i}"})
      assert_push "msg:ack", %{client_msg_id: "rh-" <> _}
    end

    # A new socket (same user, e.g. after reconnect) gets the full
    # history oldest-first in the join reply.
    socket2 = socket(SutraChatWeb.UserSocket, "user_socket:#{@user}", %{user_id: @user})

    assert {:ok, %{history: %{messages: messages, next_cursor: nil}}, _} =
             subscribe_and_join(socket2, SutraChatWeb.ChatChannel, "room:#{room}")

    assert Enum.map(messages, & &1.body) == ["one", "two", "three"]
  end

  test "history paginates newest-first with opaque cursors", %{socket: socket} do
    room = "paged-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    for i <- 1..5 do
      push(socket, "message:new", %{"body" => "msg-#{i}", "client_msg_id" => "pg-#{i}"})
      assert_push "msg:ack", %{client_msg_id: "pg-" <> _}
    end

    assert {:ok, %{messages: p1, next_cursor: c1}} = Chat.history(room, 2)
    assert Enum.map(p1, & &1.body) == ["msg-4", "msg-5"]
    assert is_binary(c1)

    assert {:ok, %{messages: p2, next_cursor: c2}} = Chat.history(room, 2, c1)
    assert Enum.map(p2, & &1.body) == ["msg-2", "msg-3"]
    assert is_binary(c2)

    assert {:ok, %{messages: p3, next_cursor: nil}} = Chat.history(room, 2, c2)
    assert Enum.map(p3, & &1.body) == ["msg-1"]

    assert {:error, :invalid_cursor} = Chat.history(room, 2, "garbage")
  end

  # -- payload validation -------------------------------------------------

  test "oversized body is rejected with a clean error", %{socket: socket} do
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:valid-#{uniq()}")

    ref =
      push(socket, "message:new", %{
        "body" => String.duplicate("x", 4097),
        "client_msg_id" => "big-1"
      })

    assert_reply ref, :error, %{reason: "message too long", max_length: 4096}

    # Nothing was persisted.
    assert {:ok, %{messages: []}} = Chat.history("valid-#{uniq()}")
  end

  test "missing body or client_msg_id is rejected", %{socket: socket} do
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:valid2-#{uniq()}")

    ref = push(socket, "message:new", %{"body" => "no id here"})
    assert_reply ref, :error, %{reason: "body and client_msg_id required"}

    ref2 = push(socket, "message:new", %{"body" => nil, "client_msg_id" => "nil-body"})
    assert_reply ref2, :error, %{reason: "body and client_msg_id required"}
  end

  # -- helpers ------------------------------------------------------------

  defp uniq, do: System.unique_integer([:positive])
end
