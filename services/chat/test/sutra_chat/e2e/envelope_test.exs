# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.E2E.EnvelopeTest do
  @moduledoc """
  Pure unit tests for the E2E Double-Ratchet message envelope validation
  (I3). The server performs no ratchet computation — these tests pin the
  transport contract: required fields, exact ratchet-key size, sane
  header counters, and the ciphertext bounds that keep one message from
  becoming a storage/amplification vector.
  """
  use ExUnit.Case, async: true

  alias SutraChat.E2E.Envelope

  defp valid_params(overrides \\ %{}) do
    Map.merge(
      %{
        "recipient" => "bob-subject",
        "sender_device_id" => "alice-phone",
        "ratchet_key" => Base.encode64(:crypto.strong_rand_bytes(32)),
        "prev_chain_length" => 0,
        "message_number" => 3,
        "ciphertext" => Base.encode64(:crypto.strong_rand_bytes(128))
      },
      overrides
    )
  end

  test "accepts a well-formed envelope and stamps the protocol version" do
    assert {:ok, env} = Envelope.validate_message(valid_params())
    assert env["version"] == Envelope.protocol_version()
    assert env["recipient"] == "bob-subject"
    assert env["sender_device_id"] == "alice-phone"
    assert env["prev_chain_length"] == 0
    assert env["message_number"] == 3
    assert {:ok, <<_::256>>} = Base.decode64(env["ratchet_key"])
    assert {:ok, <<_::binary-size(128)>>} = Base.decode64(env["ciphertext"])
  end

  test "counters accept zero and large uint32 values" do
    assert {:ok, env} = Envelope.validate_message(valid_params(%{"prev_chain_length" => 4_294_967_295, "message_number" => 0}))
    assert env["prev_chain_length"] == 4_294_967_295
  end

  test "non-map payloads are rejected" do
    assert {:error, :invalid_payload} = Envelope.validate_message(nil)
    assert {:error, :invalid_payload} = Envelope.validate_message("nope")
    assert {:error, :invalid_payload} = Envelope.validate_message([])
  end

  test "missing fields are rejected" do
    # String fields report :missing_field; the counter fields report
    # :bad_counter (nil is not a valid counter) — both are rejections.
    for field <- ~w(recipient sender_device_id ratchet_key ciphertext) do
      assert {:error, :missing_field} = Envelope.validate_message(Map.delete(valid_params(), field)),
             "field #{field} should be required"
    end

    for field <- ~w(prev_chain_length message_number) do
      assert {:error, :bad_counter} = Envelope.validate_message(Map.delete(valid_params(), field)),
             "field #{field} should be required"
    end
  end

  test "empty recipient and empty sender_device_id are rejected" do
    assert {:error, :missing_field} = Envelope.validate_message(valid_params(%{"recipient" => ""}))
    assert {:error, :missing_field} = Envelope.validate_message(valid_params(%{"sender_device_id" => ""}))
  end

  test "oversized recipient and sender_device_id are rejected" do
    assert {:error, :field_too_long} = Envelope.validate_message(valid_params(%{"recipient" => String.duplicate("x", 257)}))
    assert {:error, :field_too_long} = Envelope.validate_message(valid_params(%{"sender_device_id" => String.duplicate("x", 129)}))
  end

  test "ratchet key must be valid base64 of exactly 32 bytes" do
    assert {:error, :bad_base64} = Envelope.validate_message(valid_params(%{"ratchet_key" => "!!!not-base64!!!"}))
    assert {:error, :bad_key_size} = Envelope.validate_message(valid_params(%{"ratchet_key" => Base.encode64(:crypto.strong_rand_bytes(31))}))
    assert {:error, :bad_key_size} = Envelope.validate_message(valid_params(%{"ratchet_key" => Base.encode64(:crypto.strong_rand_bytes(33))}))
  end

  test "counters must be non-negative integers within uint32" do
    assert {:error, :bad_counter} = Envelope.validate_message(valid_params(%{"message_number" => -1}))
    assert {:error, :bad_counter} = Envelope.validate_message(valid_params(%{"prev_chain_length" => 4_294_967_296}))
    assert {:error, :bad_counter} = Envelope.validate_message(valid_params(%{"message_number" => 1.5}))
    assert {:error, :bad_counter} = Envelope.validate_message(valid_params(%{"message_number" => "3"}))
  end

  test "ciphertext must be non-empty and within 64KiB" do
    assert {:error, :ciphertext_empty} = Envelope.validate_message(valid_params(%{"ciphertext" => Base.encode64("")}))
    assert {:error, :bad_base64} = Envelope.validate_message(valid_params(%{"ciphertext" => "not valid base64 !!"}))
    big = Base.encode64(:crypto.strong_rand_bytes(65_537))
    assert {:error, :ciphertext_too_large} = Envelope.validate_message(valid_params(%{"ciphertext" => big}))
  end

  test "key bytes are re-encoded canonically" do
    # Standard base64 with padding is the wire form (same convention as
    # the I1/I2 endpoints); the stored envelope always carries the
    # canonical re-encoding of the decoded bytes.
    raw = :crypto.strong_rand_bytes(32)
    assert {:ok, env} = Envelope.validate_message(valid_params(%{"ratchet_key" => Base.encode64(raw)}))
    assert env["ratchet_key"] == Base.encode64(raw)
    assert {:ok, ^raw} = Base.decode64(env["ratchet_key"])
  end

  test "limits/0 documents the envelope bounds" do
    limits = Envelope.limits()
    assert limits.key_bytes == 32
    assert limits.max_ciphertext_bytes == 65_536
    assert limits.max_counter == 4_294_967_295
  end
end
