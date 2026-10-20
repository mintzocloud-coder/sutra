# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Chat.Message do
  @moduledoc """
  A single persisted chat message.

  Identity: `id` is a server-generated UUID. Deduplication key for idempotent
  sends is `(sender_subject, client_msg_id)` — the client-generated id travels
  with the send and survives reconnects, so a retried send can never create a
  second row.
  """
  use Ecto.Schema
  import Ecto.Changeset

  @primary_key {:id, :binary_id, autogenerate: true}
  @foreign_key_type :binary_id
  schema "messages" do
    field :room_id, :string
    field :sender_subject, :string
    field :client_msg_id, :string
    field :body, :string

    # I2: 'text' = plain-text message, 'e2e_session_init' = the opaque
    # X3DH session-establishment envelope (ciphertext + public keys only).
    # I3: 'e2e_message' = the opaque Double-Ratchet message envelope
    # (ratchet header + AEAD ciphertext, never plaintext).
    # Rows of e2e kinds carry a base64 ciphertext payload, so the body
    # cap is kind-dependent (see body_length_cap/1).
    field :kind, :string, default: "text"

    # Microsecond precision gives a stable server-side ordering; the
    # (inserted_at, id) pair is the history cursor.
    timestamps(type: :utc_datetime_usec, updated_at: false)
  end

  @required_fields ~w(room_id sender_subject client_msg_id body)a

  @known_kinds ~w(text e2e_session_init e2e_message)

  # Plain text keeps Telegram's 4096-char cap. An e2e_session_init or
  # e2e_message row carries a JSON envelope with base64 keys plus a
  # base64 AEAD ciphertext (server cap 64KiB plaintext-ciphertext): the
  # cap covers the worst case with JSON overhead, and the handshake /
  # envelope validators enforce the per-field byte limits first.
  @body_length_cap_by_kind %{"text" => 4_096, "e2e_session_init" => 96_000, "e2e_message" => 96_000}

  @doc false
  def changeset(message, attrs) do
    message
    |> cast(attrs, @required_fields ++ [:kind])
    |> validate_required(@required_fields)
    |> validate_inclusion(:kind, @known_kinds)
    # Size caps mirror SutraChat.Chat.RoomPolicy (the channel rejects
    # oversized payloads first, but the persistence boundary must never
    # depend on the channel having checked).
    |> validate_body_length()
    |> validate_length(:client_msg_id, max: 128)
    # The unique index backs the INSERT ... ON CONFLICT in Chat.persist_message/1.
    |> unique_constraint([:sender_subject, :client_msg_id],
      name: :messages_sender_subject_client_msg_id_index
    )
  end

  # Kind-dependent body cap: the default validate_length/3 cannot branch
  # on another field, so this runs the cap as a custom validation.
  defp validate_body_length(changeset) do
    kind = get_field(changeset, :kind) || "text"
    cap = Map.get(@body_length_cap_by_kind, kind, 4_096)

    validate_change(changeset, :body, fn :body, body ->
      if is_binary(body) and String.length(body) > cap do
        [body: "should be at most #{cap} character(s)"]
      else
        []
      end
    end)
  end

  @doc "Known message kinds, for tests and clients."
  @spec known_kinds() :: [String.t()]
  def known_kinds, do: @known_kinds
end
