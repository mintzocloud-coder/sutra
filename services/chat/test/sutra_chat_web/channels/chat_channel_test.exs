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
    # The join reply echoes the persistence key (the room id WITHOUT the
    # "room:" topic prefix) — the same value broadcasts, history cursors,
    # and the smoke script's join assertion all use (see scripts/smoke_ws.py).
    room = "fresh-#{uniq()}"

    assert {:ok, %{room_id: ^room, history: %{messages: [], next_cursor: nil}}, _} =
             subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")
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

  # -- e2e:session_init (I2: X3DH handshake transport) --------------------

  test "session_init persists the envelope, broadcasts, and acks", %{socket: socket} do
    room = "e2e-init-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    push(socket, "e2e:session_init", init_params("peer-#{uniq()}", "init-1"))

    # Room subscribers see the handshake under its own event name, with
    # the kind marker so clients know body is a JSON envelope.
    assert_broadcast "e2e:session_init", broadcast
    assert broadcast.kind == "e2e_session_init"
    assert broadcast.client_msg_id == "init-1"
    assert broadcast.user_id == @user
    assert is_binary(broadcast.id)

    envelope = Jason.decode!(broadcast.body)
    assert envelope["version"] == 1
    assert envelope["recipient"] =~ "peer-"
    assert {:ok, <<_::256>>} = Base.decode64(envelope["identity_key"])
    assert {:ok, <<_::256>>} = Base.decode64(envelope["base_key"])
    assert {:ok, _} = Base.decode64(envelope["ciphertext"])

    # Sender-only ack, exactly like message:new.
    assert_push "msg:ack", ack
    assert ack.deduped == false
    assert ack.id == broadcast.id

    # The row is really in PostgreSQL with the E2E kind: an offline
    # recipient replays it from history — required, because their first
    # ratchet step is derived from exactly this message.
    assert {:ok, %{messages: [stored]}} = Chat.history(room)
    assert stored.kind == "e2e_session_init"
    assert stored.body == broadcast.body
  end

  test "retried session_init with the same client_msg_id is deduped", %{socket: socket} do
    room = "e2e-dedupe-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    params = init_params("peer-#{uniq()}", "init-dup")
    push(socket, "e2e:session_init", params)
    assert_push "msg:ack", %{deduped: false, id: first_id}

    # A retry (e.g. the ack was lost) must not create a second session:
    # no new row, ack carries deduped: true with the ORIGINAL id.
    push(socket, "e2e:session_init", params)
    assert_push "msg:ack", %{deduped: true, id: ^first_id}

    assert {:ok, %{messages: messages}} = Chat.history(room)
    assert length(messages) == 1
    assert hd(messages).id == first_id
    assert hd(messages).kind == "e2e_session_init"
  end

  test "offline recipient replays the handshake from history", %{socket: socket} do
    room = "e2e-replay-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    push(socket, "e2e:session_init", init_params("peer-#{uniq()}", "init-replay"))
    assert_push "msg:ack", %{deduped: false}

    # A fresh socket (Bob, coming online later) gets the init message in
    # the join reply — session establishment cannot depend on being
    # online at send time.
    socket2 = socket(SutraChatWeb.UserSocket, "user_socket:#{@user}", %{user_id: @user})

    assert {:ok, %{history: %{messages: [replayed]}}, _} =
             subscribe_and_join(socket2, SutraChatWeb.ChatChannel, "room:#{room}")

    assert replayed.kind == "e2e_session_init"
    assert Jason.decode!(replayed.body)["client_msg_id"] == "init-replay"
  end

  test "malformed handshake envelopes are rejected and never persisted", %{socket: socket} do
    room = "e2e-bad-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    base = init_params("peer-#{uniq()}", "init-bad")

    cases = [
      {Map.put(base, "identity_key", "!!!not-base64!!!"), "handshake key field is not valid base64"},
      {Map.put(base, "base_key", Base.encode64(:crypto.strong_rand_bytes(31))),
       "handshake public key must be 32 bytes"},
      {Map.delete(base, "ciphertext"), "handshake envelope missing a required field"},
      {Map.put(base, "registration_id", 0), "registration_id must be 1..65535"},
      {Map.put(base, "ciphertext", Base.encode64(:crypto.strong_rand_bytes(65_537))),
       "handshake ciphertext too large"},
      {%{"junk" => true}, "handshake envelope missing a required field"}
    ]

    for {params, reason} <- cases do
      ref = push(socket, "e2e:session_init", params)
      assert_reply ref, :error, %{reason: ^reason}
    end

    # Nothing reached the database.
    assert {:ok, %{messages: []}} = Chat.history(room)
  end

  # -- e2e_message (I3: Double-Ratchet message transport) -----------------

  defp e2e_msg_params(recipient, client_msg_id) do
    %{
      "e2e_data" => %{
        "recipient" => recipient,
        "sender_device_id" => "test-device",
        "ratchet_key" => Base.encode64(:crypto.strong_rand_bytes(32)),
        "prev_chain_length" => 2,
        "message_number" => 5,
        "ciphertext" => Base.encode64(:crypto.strong_rand_bytes(96))
      },
      "client_msg_id" => client_msg_id
    }
  end

  test "e2e message persists ciphertext, broadcasts as message:new, and acks", %{socket: socket} do
    room = "e2e-msg-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    push(socket, "message:new", e2e_msg_params("peer-#{uniq()}", "e2e-1"))

    # Same event name as plaintext sends (task contract); recipients
    # switch on `kind`.
    assert_broadcast "message:new", broadcast
    assert broadcast.kind == "e2e_message"
    assert broadcast.client_msg_id == "e2e-1"
    assert broadcast.user_id == @user
    assert is_binary(broadcast.id)

    # The stored body is the canonical envelope: ciphertext + ratchet
    # header, never plaintext.
    envelope = Jason.decode!(broadcast.body)
    assert envelope["version"] == 1
    assert envelope["recipient"] =~ "peer-"
    assert envelope["prev_chain_length"] == 2
    assert envelope["message_number"] == 5
    assert {:ok, <<_::256>>} = Base.decode64(envelope["ratchet_key"])
    assert {:ok, <<_::binary-size(96)>>} = Base.decode64(envelope["ciphertext"])

    assert_push "msg:ack", ack
    assert ack.deduped == false
    assert ack.id == broadcast.id

    assert {:ok, %{messages: [stored]}} = Chat.history(room)
    assert stored.kind == "e2e_message"
    assert stored.body == broadcast.body
  end

  test "retried e2e message with the same client_msg_id is deduped", %{socket: socket} do
    room = "e2e-msg-dedupe-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    params = e2e_msg_params("peer-#{uniq()}", "e2e-dup")
    push(socket, "message:new", params)
    assert_push "msg:ack", %{deduped: false, id: first_id}

    push(socket, "message:new", params)
    assert_push "msg:ack", %{deduped: true, id: ^first_id}

    assert {:ok, %{messages: messages}} = Chat.history(room)
    assert length(messages) == 1
    assert hd(messages).id == first_id
    assert hd(messages).kind == "e2e_message"
  end

  test "offline recipient replays the e2e message from history", %{socket: socket} do
    room = "e2e-msg-replay-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    push(socket, "message:new", e2e_msg_params("peer-#{uniq()}", "e2e-replay"))
    assert_push "msg:ack", %{deduped: false}

    socket2 = socket(SutraChatWeb.UserSocket, "user_socket:#{@user}", %{user_id: @user})

    assert {:ok, %{history: %{messages: [replayed]}}, _} =
             subscribe_and_join(socket2, SutraChatWeb.ChatChannel, "room:#{room}")

    assert replayed.kind == "e2e_message"
    assert Jason.decode!(replayed.body)["message_number"] == 5
  end

  test "mixed plaintext+e2e payload is rejected and never persisted", %{socket: socket} do
    room = "e2e-msg-mixed-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    params = Map.put(e2e_msg_params("peer-#{uniq()}", "e2e-mixed"), "body", "oops plaintext")
    ref = push(socket, "message:new", params)
    assert_reply ref, :error, %{reason: "a message is either plaintext or E2E, never both"}

    assert {:ok, %{messages: []}} = Chat.history(room)
  end

  test "malformed e2e envelopes are rejected and never persisted", %{socket: socket} do
    room = "e2e-msg-bad-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    base = e2e_msg_params("peer-#{uniq()}", "e2e-bad")

    cases = [
      {put_in(base, ["e2e_data", "ratchet_key"], "!!!not-base64!!!"),
       "e2e envelope key field is not valid base64"},
      {put_in(base, ["e2e_data", "ratchet_key"], Base.encode64(:crypto.strong_rand_bytes(31))),
       "e2e ratchet key must be 32 bytes"},
      {update_in(base, ["e2e_data"], &Map.delete(&1, "ciphertext")),
       "e2e envelope missing a required field"},
      {put_in(base, ["e2e_data", "message_number"], -1),
       "e2e envelope counters must be non-negative integers"},
      {put_in(base, ["e2e_data", "ciphertext"], Base.encode64(:crypto.strong_rand_bytes(65_537))),
       "e2e ciphertext too large"},
      {%{"e2e_data" => %{"junk" => true}, "client_msg_id" => "e2e-bad-2"},
       "e2e envelope missing a required field"},
      {%{"e2e_data" => e2e_msg_params("peer-#{uniq()}", "x")["e2e_data"]},
       "client_msg_id required"}
    ]

    for {params, reason} <- cases do
      ref = push(socket, "message:new", params)
      assert_reply ref, :error, %{reason: ^reason}
    end

    assert {:ok, %{messages: []}} = Chat.history(room)
  end

  test "join serves the per-conversation E2E flag", %{socket: socket} do
    SutraChat.DataCase.create_e2e_conversations_table!()

    room = "e2e-flag-#{uniq()}"
    {:ok, _, socket} = subscribe_and_join(socket, SutraChatWeb.ChatChannel, "room:#{room}")

    # Before any session establishment: not E2E.
    socket2 = socket(SutraChatWeb.UserSocket, "user_socket:#{@user}", %{user_id: @user})

    assert {:ok, %{e2e_enabled: false}, _} =
             subscribe_and_join(socket2, SutraChatWeb.ChatChannel, "room:#{room}")

    # The first persisted session_init flips the flag (evidence-based).
    push(socket, "e2e:session_init", init_params("peer-#{uniq()}", "flag-init"))
    assert_push "msg:ack", %{deduped: false}

    socket3 = socket(SutraChatWeb.UserSocket, "user_socket:#{@user}", %{user_id: @user})

    assert {:ok, %{e2e_enabled: true}, _} =
             subscribe_and_join(socket3, SutraChatWeb.ChatChannel, "room:#{room}")
  end

  # -- helpers ------------------------------------------------------------

  defp uniq, do: System.unique_integer([:positive])

  defp init_params(recipient, client_msg_id) do
    %{
      "recipient" => recipient,
      "client_msg_id" => client_msg_id,
      "sender_device_id" => "test-device",
      "registration_id" => 4242,
      "identity_key" => Base.encode64(:crypto.strong_rand_bytes(32)),
      "base_key" => Base.encode64(:crypto.strong_rand_bytes(32)),
      "signed_prekey_id" => 7,
      "prekey_id" => 3,
      "ciphertext" => Base.encode64(:crypto.strong_rand_bytes(128))
    }
  end
end
