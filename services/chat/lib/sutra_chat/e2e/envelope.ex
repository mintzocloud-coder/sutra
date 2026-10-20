# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.E2E.Envelope do
  @moduledoc """
  Pure validation for the E2E Double-Ratchet message envelope (I3).

  After X3DH session establishment (I2), every 1:1 message travels as
  ciphertext inside an `e2e_data` envelope pushed with the `message:new`
  channel event. The server performs NO ratchet computation — the envelope
  is opaque to it — but it DOES validate the transport shape (exact key
  sizes, sane counters, ciphertext size-capped) and persists the canonical
  envelope with message kind `"e2e_message"`. The messages table therefore
  holds **ciphertext only, never plaintext**, for E2E rows.

  A single send is either plaintext (`body`, kind `"text"`) or E2E
  (`e2e_data`, kind `"e2e_message"`) — never both. The channel rejects a
  push that carries both, so a confused client cannot leak plaintext into
  an E2E conversation through a mixed payload.

  Envelope fields (all required):

    * `"recipient"` — the intended recipient's OIDC subject, so the right
      device picks the envelope out of the room broadcast.
    * `"sender_device_id"` — the sending device label (1..128 bytes).
    * `"ratchet_key"` — the sender's current Double-Ratchet ephemeral DH
      public key (exactly 32 bytes, base64). The recipient uses it for
      the DH ratchet step on direction change.
    * `"prev_chain_length"` — PN in Signal's header: the length of the
      previous sending chain (non-negative integer, uint32-capped).
    * `"message_number"` — N in Signal's header: the position in the
      current sending chain (non-negative integer, uint32-capped).
    * `"ciphertext"` — the XChaCha20-Poly1305 AEAD ciphertext of the
      message (base64, 1 byte..64KiB decoded). Opaque to the server.

  Protocol profile version 1 — the audited-construction table in
  `docs/e2e.md` (X25519 / HKDF-SHA256 / XChaCha20-Poly1305 / Ed25519)
  applies unchanged; clients MUST use exactly those constructions.

  Kept pure (no Repo, no Phoenix) so the validation is unit-testable
  without a database or a compiled Phoenix.
  """

  @protocol_version 1

  # The ratchet DH public key is X25519: exactly 32 bytes, no more, no
  # less — same size-first rejection rationale as the handshake keys.
  @key_bytes 32
  # One ratchet message's AEAD ciphertext: bounded so a single send can
  # never become a storage/amplification vector.
  @max_ciphertext_bytes 65_536
  @max_device_id 128
  @max_subject 256
  # Header counters are uint32; the envelope is stored as JSON so there
  # is no DB column to overflow, but unbounded counters would be a
  # protocol smell — cap them.
  @max_counter 4_294_967_295

  @typedoc "E2E message-envelope failure reasons, surfaced to clients as `reason` strings."
  @type envelope_error ::
          :invalid_payload
          | :missing_field
          | :field_too_long
          | :bad_base64
          | :bad_key_size
          | :bad_counter
          | :ciphertext_empty
          | :ciphertext_too_large

  @doc """
  Validates and canonicalizes an `e2e_data` push payload.

  Returns `{:ok, envelope}` — the canonical map the channel persists
  (as JSON, kind `"e2e_message"`) and broadcasts — or `{:error, reason}`.
  Key bytes and ciphertext are re-encoded to canonical standard base64
  (with padding) on the way through.
  """
  @spec validate_message(term()) :: {:ok, map()} | {:error, envelope_error()}
  def validate_message(params) when is_map(params) do
    with {:ok, recipient} <- fetch_subject(params, "recipient"),
         {:ok, sender_device_id} <- fetch_bounded(params, "sender_device_id", @max_device_id),
         {:ok, ratchet_key} <- fetch_key(params, "ratchet_key"),
         {:ok, prev_chain_length} <- fetch_counter(params, "prev_chain_length"),
         {:ok, message_number} <- fetch_counter(params, "message_number"),
         {:ok, ciphertext} <- fetch_ciphertext(params) do
      {:ok,
       %{
         "version" => @protocol_version,
         "recipient" => recipient,
         "sender_device_id" => sender_device_id,
         "ratchet_key" => Base.encode64(ratchet_key),
         "prev_chain_length" => prev_chain_length,
         "message_number" => message_number,
         "ciphertext" => Base.encode64(ciphertext)
       }}
    end
  end

  def validate_message(_params), do: {:error, :invalid_payload}

  @doc "Protocol profile version stamped into every stored envelope."
  @spec protocol_version() :: pos_integer()
  def protocol_version, do: @protocol_version

  @doc "Documented envelope limits, for tests and clients."
  @spec limits() :: map()
  def limits do
    %{
      key_bytes: @key_bytes,
      max_ciphertext_bytes: @max_ciphertext_bytes,
      max_device_id: @max_device_id,
      max_subject: @max_subject,
      max_counter: @max_counter
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

  # The ratchet DH public key: base64 must decode AND be exactly 32 bytes.
  defp fetch_key(params, field) do
    with {:ok, raw} <- decode_field(params, field),
         true <- byte_size(raw) == @key_bytes do
      {:ok, raw}
    else
      {:error, _} = err -> err
      false -> {:error, :bad_key_size}
    end
  end

  # Header counters: non-negative integers, uint32-capped. A negative or
  # fractional counter is never a valid Signal header — reject loudly.
  defp fetch_counter(params, field) do
    case Map.get(params, field) do
      n when is_integer(n) and n >= 0 and n <= @max_counter -> {:ok, n}
      _ -> {:error, :bad_counter}
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
