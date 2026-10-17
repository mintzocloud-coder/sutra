# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Chat.RoomPolicyTest do
  use ExUnit.Case, async: true

  alias SutraChat.Chat.RoomPolicy

  describe "authorize_join/2" do
    test "authenticated subject + well-formed room id → :ok" do
      assert :ok = RoomPolicy.authorize_join("sub-123", "general")
      assert :ok = RoomPolicy.authorize_join("sub-123", "room_9-aB")
    end

    test "unauthenticated join → :unauthorized (nil and empty)" do
      assert {:error, :unauthorized} = RoomPolicy.authorize_join(nil, "general")
      assert {:error, :unauthorized} = RoomPolicy.authorize_join("", "general")
    end

    test "malformed room ids → :invalid_room_id" do
      for bad <- ["", "room id", "room/id", "room:id", "room!", "général",
                  String.duplicate("a", 65), "..", "../etc/passwd"] do
        assert {:error, :invalid_room_id} = RoomPolicy.authorize_join("sub-123", bad),
               "expected invalid for #{inspect(bad)}"
      end
    end

    test "boundary: 64 chars ok, 65 rejected" do
      assert :ok = RoomPolicy.authorize_join("sub-123", String.duplicate("a", 64))
      assert {:error, :invalid_room_id} = RoomPolicy.authorize_join("sub-123", String.duplicate("a", 65))
    end
  end

  describe "authorize_send/2" do
    test "normal payload → :ok" do
      assert :ok = RoomPolicy.authorize_send("hello", "client-1")
    end

    test "missing body or client_msg_id → :invalid_payload" do
      assert {:error, :invalid_payload} = RoomPolicy.authorize_send("", "client-1")
      assert {:error, :invalid_payload} = RoomPolicy.authorize_send("hello", "")
      assert {:error, :invalid_payload} = RoomPolicy.authorize_send(nil, "client-1")
      assert {:error, :invalid_payload} = RoomPolicy.authorize_send("hello", nil)
      assert {:error, :invalid_payload} = RoomPolicy.authorize_send(42, "client-1")
    end

    test "oversized body → :body_too_long" do
      ok_body = String.duplicate("a", 4096)
      big_body = String.duplicate("a", 4097)
      assert :ok = RoomPolicy.authorize_send(ok_body, "client-1")
      assert {:error, :body_too_long} = RoomPolicy.authorize_send(big_body, "client-1")
    end

    test "oversized client_msg_id → :client_msg_id_too_long" do
      assert {:error, :client_msg_id_too_long} =
               RoomPolicy.authorize_send("hello", String.duplicate("c", 129))
    end

    test "limit is grapheme-based, not byte-based (emoji counts as one)" do
      body = String.duplicate("😀", 4097)
      assert {:error, :body_too_long} = RoomPolicy.authorize_send(body, "client-1")
    end
  end

  describe "limits/0" do
    test "exposes documented caps" do
      assert %{body: 4096, client_msg_id: 128, room_id: 64} = RoomPolicy.limits()
    end
  end
end
