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

    # Microsecond precision gives a stable server-side ordering; the
    # (inserted_at, id) pair is the history cursor.
    timestamps(type: :utc_datetime_usec, updated_at: false)
  end

  @required_fields ~w(room_id sender_subject client_msg_id body)a

  @doc false
  def changeset(message, attrs) do
    message
    |> cast(attrs, @required_fields)
    |> validate_required(@required_fields)
    # Size caps mirror SutraChat.Chat.RoomPolicy (the channel rejects
    # oversized payloads first, but the persistence boundary must never
    # depend on the channel having checked).
    |> validate_length(:body, max: 4096)
    |> validate_length(:client_msg_id, max: 128)
    # The unique index backs the INSERT ... ON CONFLICT in Chat.persist_message/1.
    |> unique_constraint([:sender_subject, :client_msg_id],
      name: :messages_sender_subject_client_msg_id_index
    )
  end
end
