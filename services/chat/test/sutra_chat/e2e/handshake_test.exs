# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.E2E.HandshakeTest do
  @moduledoc """
  Pure unit tests for the X3DH session-establishment envelope validation
  (I2). The server performs no crypto — these tests pin the transport
  contract: required fields, exact key sizes, base64 discipline, and the
  ciphertext bounds that keep one handshake from becoming a
  storage/amplification vector.
  """
  use ExUnit.Case, async: true

  alias SutraChat.E2E.Handshake

  defp valid_params(overrides \\ %{}) do
    Map.merge(
      %{
        "recipient" => "bob-subject",
        "client_msg_id" => "init-1",
        "sender_device_id" => "alice-phone",
        "registration_id" => 4242,
        "identity_key" => Base.encode64(:crypto.strong_rand_bytes(32)),
        "base_key" => Base.encode64(:crypto.strong_rand_bytes(32)),
        "signed_prekey_id" => 7,
        "prekey_id" => 3,
        "ciphertext" => Base.encode64(:crypto.strong_rand_bytes(128))
      },
      overrides
    )
  end

  test "accepts a well-formed envelope and stamps the protocol version" do
    assert {:ok, env} = Handshake.validate_init(valid_params())
    assert env["version"] == Handshake.protocol_version()
    assert env["recipient"] == "bob-subject"
    assert env["prekey_id"] == 3
    # Key bytes round-trip through canonical base64.
    assert {:ok, <<_::256>>} = Base.decode64(env["identity_key"])
    assert {:ok, <<_::256>>} = Base.decode64(env["base_key"])
    assert {:ok, <<_::binary-size(128)>>} = Base.decode64(env["ciphertext"])
  end

  test "prekey_id may be null (signed-prekey-only X3DH fallback)" do
    assert {:ok, env} = Handshake.validate_init(valid_params(%{"prekey_id" => nil}))
    assert env["prekey_id"] == nil
  end

  test "non-map payloads are rejected" do
    assert {:error, :invalid_payload} = Handshake.validate_init(nil)
    assert {:error, :invalid_payload} = Handshake.validate_init("nope")
    assert {:error, :invalid_payload} = Handshake.validate_init([])
  end

  test "missing required fields are named" do
    for field <- ~w(recipient client_msg_id sender_device_id identity_key
                    base_key ciphertext) do
      assert {:error, :missing_field} =
               Handshake.validate_init(Map.delete(valid_params(), field)),
             "field #{field} should be required"
    end
  end

  test "non-base64 key material is rejected" do
    assert {:error, :bad_base64} =
             Handshake.validate_init(valid_params(%{"identity_key" => "!!!not-base64!!!"}))

    assert {:error, :bad_base64} =
             Handshake.validate_init(valid_params(%{"ciphertext" => "!!!not-base64!!!"}))
  end

  test "public keys must be exactly 32 bytes" do
    for field <- ~w(identity_key base_key) do
      short = Base.encode64(:crypto.strong_rand_bytes(31))
      long = Base.encode64(:crypto.strong_rand_bytes(33))

      assert {:error, :bad_key_size} =
               Handshake.validate_init(valid_params(%{field => short})), "short #{field}"

      assert {:error, :bad_key_size} =
               Handshake.validate_init(valid_params(%{field => long})), "long #{field}"
    end
  end

  test "ciphertext must be non-empty and within the cap" do
    assert {:error, :ciphertext_empty} =
             Handshake.validate_init(valid_params(%{"ciphertext" => Base.encode64("")}))

    assert {:error, :missing_field} =
             Handshake.validate_init(Map.delete(valid_params(), "ciphertext"))

    big = Base.encode64(:crypto.strong_rand_bytes(65_537))

    assert {:error, :ciphertext_too_large} =
             Handshake.validate_init(valid_params(%{"ciphertext" => big}))

    # Boundary: exactly the cap passes validation.
    at_cap = Base.encode64(:crypto.strong_rand_bytes(65_536))
    assert {:ok, _} = Handshake.validate_init(valid_params(%{"ciphertext" => at_cap}))
  end

  test "registration_id is bounded to uint16 range" do
    for bad <- [0, -1, 65_536, "4242", nil] do
      assert {:error, :bad_registration_id} =
               Handshake.validate_init(valid_params(%{"registration_id" => bad})),
             "registration_id #{inspect(bad)} should be rejected"
    end

    assert {:ok, _} = Handshake.validate_init(valid_params(%{"registration_id" => 1}))
    assert {:ok, _} = Handshake.validate_init(valid_params(%{"registration_id" => 65_535}))
  end

  test "prekey ids are bounded" do
    for field <- ~w(signed_prekey_id prekey_id) do
      for bad <- [0, -5, 16_777_216, "7"] do
        assert {:error, :bad_prekey_id} =
                 Handshake.validate_init(valid_params(%{field => bad})),
               "#{field}=#{inspect(bad)} should be rejected"
      end
    end
  end

  test "oversized string fields are rejected" do
    long_id = String.duplicate("x", 129)

    assert {:error, :field_too_long} =
             Handshake.validate_init(valid_params(%{"client_msg_id" => long_id}))

    assert {:error, :field_too_long} =
             Handshake.validate_init(valid_params(%{"sender_device_id" => long_id}))

    assert {:error, :field_too_long} =
             Handshake.validate_init(valid_params(%{"recipient" => String.duplicate("x", 257)}))
  end

  test "empty-string fields are missing fields" do
    assert {:error, :missing_field} =
             Handshake.validate_init(valid_params(%{"recipient" => ""}))

    assert {:error, :missing_field} =
             Handshake.validate_init(valid_params(%{"client_msg_id" => ""}))
  end

  test "limits/0 documents the envelope bounds" do
    limits = Handshake.limits()
    assert limits.key_bytes == 32
    assert limits.max_ciphertext_bytes == 65_536
    assert limits.max_registration_id == 65_535
  end
end
