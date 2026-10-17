# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Chat.DeliveryTest do
  use ExUnit.Case, async: true

  alias SutraChat.Chat
  alias SutraChat.Chat.Message

  defp message(overrides \\ %{}) do
    struct!(
      Message,
      Map.merge(
        %{
          id: Ecto.UUID.generate(),
          room_id: "general",
          sender_subject: "user:123",
          client_msg_id: "cmid-1",
          body: "hello",
          inserted_at: ~U[2026-10-17 12:00:00.123456Z]
        },
        overrides
      )
    )
  end

  describe "ack_payload/2" do
    test "carries server id, room, echoed client_msg_id and sent_at in ms" do
      msg = message()
      payload = Chat.ack_payload(msg, false)

      assert payload.id == msg.id
      assert payload.room_id == "general"
      assert payload.client_msg_id == "cmid-1"
      assert payload.sent_at == DateTime.to_unix(~U[2026-10-17 12:00:00.123456Z], :millisecond)
      assert is_integer(payload.sent_at)
    end

    test "deduped flag distinguishes a fresh send from a retry" do
      assert Chat.ack_payload(message(), false).deduped == false
      assert Chat.ack_payload(message(), true).deduped == true
    end

    test "deduped must be a boolean" do
      assert_raise FunctionClauseError, fn -> Chat.ack_payload(message(), nil) end
    end
  end
end
