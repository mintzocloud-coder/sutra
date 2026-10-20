# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.E2ETest do
  @moduledoc """
  Tests for the chat-side E2E prekey-bundle fetch (I1, part 2b).

  The `e2e_devices` / `e2e_one_time_prekeys` tables are Go-owned in
  production (migration 0021); here they are minimal test-local copies
  (see `SutraChat.DataCase.create_e2e_tables!/0`) carrying exactly the
  columns the context queries.
  """
  use SutraChat.DataCase, async: false

  alias SutraChat.E2E

  setup do
    create_e2e_tables!()
    :ok
  end

  test "fetch returns the bundle and consumes the lowest-id prekey first" do
    keys = insert_e2e_device!("bob", "pixel-9")

    assert {:ok, bundle} = E2E.fetch_prekey_bundle("bob", "pixel-9")

    assert bundle["device_id"] == "pixel-9"
    assert bundle["registration_id"] == 4242
    assert bundle["signed_prekey_id"] == 7
    assert {:ok, ikey} = Base.decode64(bundle["identity_key"])
    assert ikey == keys.identity_key
    assert {:ok, spk} = Base.decode64(bundle["signed_prekey"])
    assert spk == keys.signed_prekey
    assert {:ok, sig} = Base.decode64(bundle["signed_prekey_sig"])
    assert sig == keys.signed_prekey_sig
    assert bundle["one_time_prekey"]["id"] == 1
    assert {:ok, pk} = Base.decode64(bundle["one_time_prekey"]["public_key"])
    assert byte_size(pk) == 32
    assert bundle["available_prekeys"] == 2

    # The next fetch hands out the NEXT prekey — each one-time prekey
    # is handed out at most once.
    assert {:ok, bundle2} = E2E.fetch_prekey_bundle("bob", "pixel-9")
    assert bundle2["one_time_prekey"]["id"] == 2
    assert bundle2["available_prekeys"] == 1
    assert bundle2["one_time_prekey"]["public_key"] != bundle["one_time_prekey"]["public_key"]
  end

  test "a dry queue still returns the bundle with a nil one-time prekey" do
    insert_e2e_device!("carol", "iphone", [9])

    assert {:ok, b1} = E2E.fetch_prekey_bundle("carol", "iphone")
    assert b1["one_time_prekey"]["id"] == 9
    assert b1["available_prekeys"] == 0

    # Signed-prekey-only X3DH fallback: bundle present, no prekey.
    assert {:ok, b2} = E2E.fetch_prekey_bundle("carol", "iphone")
    assert b2["one_time_prekey"] == nil
    assert b2["device_id"] == "iphone"
    assert b2["available_prekeys"] == 0
  end

  test "an empty queue from the start returns a nil one-time prekey" do
    insert_e2e_device!("dave", "laptop", [])

    assert {:ok, bundle} = E2E.fetch_prekey_bundle("dave", "laptop")
    assert bundle["one_time_prekey"] == nil
    assert bundle["available_prekeys"] == 0
  end

  test "unknown subject or device is :not_found (no existence oracle)" do
    insert_e2e_device!("erin", "tablet")

    assert {:error, :not_found} = E2E.fetch_prekey_bundle("mallory", "tablet")
    assert {:error, :not_found} = E2E.fetch_prekey_bundle("erin", "ghost-device")
    assert {:error, :not_found} = E2E.fetch_prekey_bundle("mallory", "ghost-device")
  end

  test "malformed subject/device_id is :invalid_request" do
    assert {:error, :invalid_request} = E2E.fetch_prekey_bundle("", "pixel-9")
    assert {:error, :invalid_request} = E2E.fetch_prekey_bundle("bob", "")
    assert {:error, :invalid_request} = E2E.fetch_prekey_bundle("bob", String.duplicate("d", 65))
    assert {:error, :invalid_request} = E2E.fetch_prekey_bundle(nil, "pixel-9")
    assert {:error, :invalid_request} = E2E.fetch_prekey_bundle("bob", nil)
  end

  test "device ids are scoped to their owner" do
    insert_e2e_device!("frank", "phone")
    insert_e2e_device!("grace", "phone")

    assert {:ok, fb} = E2E.fetch_prekey_bundle("frank", "phone")
    assert {:ok, gb} = E2E.fetch_prekey_bundle("grace", "phone")
    assert fb["identity_key"] != gb["identity_key"]
  end

  # -- per-conversation E2E flag (I3) ------------------------------------

  describe "per-conversation E2E flag" do
    setup do
      create_e2e_conversations_table!()
      :ok
    end

    test "unknown room is not E2E (never claim E2E where it is not enabled)" do
      refute E2E.conversation_e2e_enabled?("ghost-room")
    end

    test "malformed room ids are not E2E and are rejected by the writer" do
      refute E2E.conversation_e2e_enabled?("bad!!room")
      refute E2E.conversation_e2e_enabled?(nil)
      assert {:error, :invalid_room_id} = E2E.mark_conversation_e2e("bad!!room", Ecto.UUID.generate())
    end

    test "marking flips the flag and pins the first init message id" do
      first = Ecto.UUID.generate()
      assert :ok = E2E.mark_conversation_e2e("room-1", first)
      assert E2E.conversation_e2e_enabled?("room-1")

      %{rows: [[enabled, anchor]]} =
        SutraChat.Repo.query!(
          "SELECT e2e_enabled, first_init_message_id FROM e2e_conversations WHERE room_id = $1",
          ["room-1"]
        )

      assert enabled == true
      assert Ecto.UUID.cast!(anchor) == first
    end

    test "the flag is monotonic: later marks never move it or the anchor" do
      first = Ecto.UUID.generate()
      second = Ecto.UUID.generate()
      assert :ok = E2E.mark_conversation_e2e("room-2", first)
      # A retry with a different init id is a no-op, not a second session.
      assert :ok = E2E.mark_conversation_e2e("room-2", second)

      %{rows: [[enabled, anchor]]} =
        SutraChat.Repo.query!(
          "SELECT e2e_enabled, first_init_message_id FROM e2e_conversations WHERE room_id = $1",
          ["room-2"]
        )

      assert enabled == true
      assert Ecto.UUID.cast!(anchor) == first
    end

    test "rooms are independent" do
      assert :ok = E2E.mark_conversation_e2e("room-a", Ecto.UUID.generate())
      assert E2E.conversation_e2e_enabled?("room-a")
      refute E2E.conversation_e2e_enabled?("room-b")
    end
  end
end
