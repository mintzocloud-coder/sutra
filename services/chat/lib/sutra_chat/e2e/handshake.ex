# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.E2E.Handshake do
  @moduledoc """
  Pure validation for the X3DH session-establishment handshake transport
  (the `e2e:session_init` channel event).

  **The server performs NO X3DH or Double-Ratchet computation.** Every ECDH,
  KDF, and AEAD operation happens on the clients; the server never holds a
  private key and could not do the math even if it wanted to. What the
  server DOES, and what this module defines, is the transport contract for
  the initiator's PreKey-style initial message:

    * shape validation — every required field present, base64-decodable,
      exact byte sizes for public keys, ciphertext size-capped;
    * canonicalization — key bytes re-encoded to canonical base64 so the
      stored form is deterministic;
    * routing metadata — the intended recipient subject, so the right
      device picks the envelope out of the room broadcast.

  The channel persists the canonical envelope with kind
  `"e2e_session_init"` BEFORE broadcasting it (offline recipients replay
  it from history — required, because the recipient's first ratchet step
  is derived from exactly this message), broadcasts it as
  `"e2e:session_init"`, and acks the sender exactly like `message:new`
  (idempotent on `(sender_subject, client_msg_id)`).

  Protocol profile version 1 — see `docs/e2e.md` for the audited
  constructions clients MUST use (X25519 / HKDF-SHA256 /
  XChaCha20-Poly1305 / Ed25519) and the exact X3DH + ratchet steps.

  Kept pure (no Repo, no Phoenix) so the validation is unit-testable
  without a database or a compiled Phoenix.
  """

  @protocol_version 1

  # Public keys are X25519/Ed25519: exactly 32 bytes, no more, no less.
  @key_bytes 32
  # The initial message's AEAD ciphertext: bounded so one handshake can
  # never become a storage/amplification vector.
  @max_ciphertext_bytes 65_536
  @max_client_msg_id 128
  @max_device_id 128
  @max_subject 256
  # Signal's prekey ids are uint24-ish; registration ids are uint16.
  @max_prekey_id 16_777_215
  @max_registration_id 65_535

  @typedoc "Handshake-envelope failure reasons, surfaced to clients as `reason` strings."
  @type init_error ::
          :invalid_payload
          | :missing_field
          | :field_too_long
          | :bad_base64
          | :bad_key_size
          | :bad_registration_id
          | :bad_prekey_id
          | :ciphertext_empty
          | :ciphertext_too_large

  @doc """
  Validates and canonicalizes an `e2e:session_init` push payload.

  Returns `{:ok, envelope}` — the canonical map the channel persists and
  broadcasts — or `{:error, reason}`. Key bytes are re-encoded to canonical
  standard base64 (with padding) on the way through.
  """
  @spec validate_init(term()) :: {:ok, map()} | {:error, init_error()}
  def validate_init(params) when is_map(params) do
    with {:ok, recipient} <- fetch_subject(params, "recipient"),
         {:ok, client_msg_id} <- fetch_bounded(params, "client_msg_id", @max_client_msg_id),
         {:ok, sender_device_id} <- fetch_bounded(params, "sender_device_id", @max_device_id),
         {:ok, registration_id} <- fetch_registration_id(params),
         {:ok, identity_key} <- fetch_key(params, "identity_key"),
         {:ok, base_key} <- fetch_key(params, "base_key"),
         {:ok, signed_prekey_id} <- fetch_prekey_id(params, "signed_prekey_id"),
         {:ok, prekey_id} <- fetch_optional_prekey_id(params),
         {:ok, ciphertext} <- fetch_ciphertext(params) do
      {:ok,
       %{
         "version" => @protocol_version,
         "recipient" => recipient,
         "client_msg_id" => client_msg_id,
         "sender_device_id" => sender_device_id,
         "registration_id" => registration_id,
         "identity_key" => Base.encode64(identity_key),
         "base_key" => Base.encode64(base_key),
         "signed_prekey_id" => signed_prekey_id,
         "prekey_id" => prekey_id,
         "ciphertext" => Base.encode64(ciphertext)
       }}
    end
  end

  def validate_init(_params), do: {:error, :invalid_payload}

  @doc "Protocol profile version stamped into every stored envelope."
  @spec protocol_version() :: pos_integer()
  def protocol_version, do: @protocol_version

  @doc "Documented envelope limits, for tests and clients."
  @spec limits() :: map()
  def limits do
    %{
      key_bytes: @key_bytes,
      max_ciphertext_bytes: @max_ciphertext_bytes,
      max_client_msg_id: @max_client_msg_id,
      max_device_id: @max_device_id,
      max_subject: @max_subject,
      max_prekey_id: @max_prekey_id,
      max_registration_id: @max_registration_id
    }
  end

  # -- field fetchers ---------------------------------------------------

  defp fetch_subject(params, field) do
    case Map.get(params, field) do
      s when is_binary(s) and s != "" and byte_size(s) <= @max_subject -> {:ok, s}
      "" -> {:error, :missing_field}
      s when is_binary(s) -> {:error, :field_too_long}
      nil -> {:error, :missing_field}
      _ -> {:error, :invalid_payload}
    end
  end

  defp fetch_bounded(params, field, max) do
    case Map.get(params, field) do
      s when is_binary(s) and s != "" and byte_size(s) <= max -> {:ok, s}
      s when is_binary(s) and s != "" -> {:error, :field_too_long}
      _ -> {:error, :missing_field}
    end
  end

  defp fetch_registration_id(params) do
    case Map.get(params, "registration_id") do
      n when is_integer(n) and n >= 1 and n <= @max_registration_id -> {:ok, n}
      _ -> {:error, :bad_registration_id}
    end
  end

  defp fetch_prekey_id(params, field) do
    case Map.get(params, field) do
      n when is_integer(n) and n >= 1 and n <= @max_prekey_id -> {:ok, n}
      _ -> {:error, :bad_prekey_id}
    end
  end

  # The one-time prekey id the initiator consumed from the directory;
  # null when the queue was dry and the session fell back to
  # signed-prekey-only X3DH (kept for audit, not for crypto).
  defp fetch_optional_prekey_id(params) do
    case Map.get(params, "prekey_id") do
      nil -> {:ok, nil}
      n when is_integer(n) and n >= 1 and n <= @max_prekey_id -> {:ok, n}
      _ -> {:error, :bad_prekey_id}
    end
  end

  # Public keys: base64 must decode AND be exactly 32 bytes. Size-first
  # rejection matters — a 31- or 33-byte "key" is never a valid X25519 or
  # Ed25519 public key, and accepting it would only help an attacker
  # smuggle malformed envelopes to clients.
  defp fetch_key(params, field) do
    with {:ok, raw} <- decode_field(params, field),
         true <- byte_size(raw) == @key_bytes do
      {:ok, raw}
    else
      {:error, _} = err -> err
      false -> {:error, :bad_key_size}
    end
  end

  defp fetch_ciphertext(params) do
    with {:ok, raw} <- decode_field(params, "ciphertext") do
      cond do
        byte_size(raw) == 0 -> {:error, :ciphertext_empty}
        byte_size(raw) > @max_ciphertext_bytes -> {:error, :ciphertext_too_large}
        true -> {:ok, raw}
      end
    end
  end

  defp decode_field(params, field) do
    case Map.get(params, field) do
      s when is_binary(s) ->
        case Base.decode64(s) do
          {:ok, raw} -> {:ok, raw}
          :error -> {:error, :bad_base64}
        end

      _ ->
        {:error, :missing_field}
    end
  end
end
