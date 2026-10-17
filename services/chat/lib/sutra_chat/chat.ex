# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Chat do
  @moduledoc """
  Message persistence boundary for the chat service.

  Design targets (better than market):

    * **Idempotent sends.** `(sender_subject, client_msg_id)` is unique at the
      database level. `persist_message/1` performs a single
      `INSERT ... ON CONFLICT` — a client retrying after a dropped connection
      gets the original row back, never a duplicate. The upsert is atomic, so
      it stays correct with many chat nodes racing on the same send.
    * **Server-side ordering.** Messages carry microsecond `inserted_at`
      timestamps. History pages on the `(inserted_at, id)` cursor — never
      OFFSET, so concurrent inserts cannot shift or duplicate pages.
  """

  import Ecto.Query, warn: false

  alias SutraChat.Chat.Message
  alias SutraChat.Repo

  @default_history_limit 50
  @max_history_limit 200

  @doc """
  Persists a message idempotently.

  On a `(sender_subject, client_msg_id)` conflict the existing row is returned
  unchanged — a retried send can never create a second row.

  Returns `{:ok, message, deduped}` or `{:error, changeset}`. `deduped` is
  `true` when the row already existed (this call was a retry) and `false` on
  a genuinely new message. The channel reports it in the `msg:ack` event so
  the sender can tell "stored as new" apart from "already had it" — a
  distinction WhatsApp/Telegram acks don't make.

  Fresh-vs-conflict detection stays inside the single atomic upsert via the
  `xmax` system column in `RETURNING`: a freshly inserted tuple has
  `xmax = 0`, while the `ON CONFLICT DO UPDATE` path always produces a new
  tuple version whose `xmax` is the updating transaction's id. No follow-up
  SELECT, so there is no race between "was it new?" and the answer.

  A database failure raises (the same contract `Repo.insert!/2` offers):
  the channel then emits no ack, and the client's retry policy — see
  `docs/delivery-semantics.md` — makes the send safe to repeat.
  """
  @spec persist_message(map()) ::
          {:ok, Message.t(), boolean()} | {:error, Ecto.Changeset.t()}
  def persist_message(attrs) do
    changeset = Message.changeset(%Message{}, attrs)

    if changeset.valid? do
      upsert_message(changeset)
    else
      # Mirrors what Repo.insert/2 returns for an invalid changeset.
      {:error, %{changeset | action: :insert}}
    end
  end

  @doc """
  Builds the `msg:ack` payload the channel pushes to the sender after a
  message has been persisted AND broadcast.

  Fields (JSON-encoded):
    * `id` — server-assigned message UUID
    * `room_id` — the room the message was sent to
    * `client_msg_id` — echoed so the client can correlate the ack with
      its in-flight send
    * `deduped` — `true` when this send was a retry of an already-stored
      message (no new row was created)
    * `sent_at` — server `inserted_at` as Unix milliseconds, so the client
      can measure send latency
  """
  @spec ack_payload(Message.t(), boolean()) :: map()
  def ack_payload(%Message{} = message, deduped) when is_boolean(deduped) do
    %{
      id: message.id,
      room_id: message.room_id,
      client_msg_id: message.client_msg_id,
      deduped: deduped,
      sent_at: DateTime.to_unix(message.inserted_at, :millisecond)
    }
  end

  # -- persistence ------------------------------------------------------

  # Single-statement idempotent upsert, hand-written so the RETURNING clause
  # can carry the xmax probe. `id`/`inserted_at` are generated here because
  # Ecto's autogenerate only applies to `Repo.insert/2`.
  #
  # NOTE: the column list is coupled to
  # `priv/repo/migrations/*_create_messages.exs` — keep them in sync.
  @upsert_sql """
  INSERT INTO messages (id, room_id, sender_subject, client_msg_id, body, inserted_at)
  VALUES ($1::uuid, $2, $3, $4, $5, $6::timestamptz)
  ON CONFLICT (sender_subject, client_msg_id)
  DO UPDATE SET body = EXCLUDED.body
  RETURNING id, room_id, sender_subject, client_msg_id, body, inserted_at,
            (xmax = 0) AS inserted_new
  """

  defp upsert_message(changeset) do
    fields = Ecto.Changeset.apply_changes(changeset)

    params = [
      Ecto.UUID.generate(),
      fields.room_id,
      fields.sender_subject,
      fields.client_msg_id,
      fields.body,
      DateTime.utc_now() |> DateTime.truncate(:microsecond)
    ]

    # Raises on DB failure — the caller (channel) then sends no ack and the
    # client retries; see docs/delivery-semantics.md.
    %{rows: [[id, room_id, sender_subject, client_msg_id, body, inserted_at, inserted_new]]} =
      Ecto.Adapters.SQL.query!(Repo, @upsert_sql, params)

    message = %Message{
      id: id,
      room_id: room_id,
      sender_subject: sender_subject,
      client_msg_id: client_msg_id,
      body: body,
      inserted_at: inserted_at
    }

    {:ok, message, not inserted_new}
  end

  @doc """
  Cursor-based room history, oldest message first.

  Returns `{:ok, %{messages: [...], next_cursor: cursor | nil}}`.
  `next_cursor` is `nil` when there is nothing older to fetch; pass it back
  as `cursor` to page further into the past. An unreadable cursor returns
  `{:error, :invalid_cursor}` instead of silently returning the wrong page.
  """
  @spec history(String.t(), pos_integer(), String.t() | nil) ::
          {:ok, %{messages: [Message.t()], next_cursor: String.t() | nil}}
          | {:error, :invalid_cursor}
  def history(room_id, limit \\ @default_history_limit, cursor \\ nil) do
    limit = limit |> max(1) |> min(@max_history_limit)

    base =
      from(m in Message,
        where: m.room_id == ^room_id,
        order_by: [desc: m.inserted_at, desc: m.id],
        limit: ^limit
      )

    with {:ok, query} <- apply_cursor(base, cursor) do
      page = Repo.all(query)

      next_cursor =
        if length(page) < limit do
          nil
        else
          page |> List.last() |> encode_cursor()
        end

      {:ok, %{messages: Enum.reverse(page), next_cursor: next_cursor}}
    end
  end

  # -- cursor -----------------------------------------------------------

  defp apply_cursor(query, nil), do: {:ok, query}

  defp apply_cursor(query, cursor) when is_binary(cursor) do
    case decode_cursor(cursor) do
      {:ok, {inserted_at, id}} ->
        {:ok,
         where(
           query,
           [m],
           m.inserted_at < ^inserted_at or (m.inserted_at == ^inserted_at and m.id < ^id)
         )}

      :invalid ->
        {:error, :invalid_cursor}
    end
  end

  defp apply_cursor(_query, _cursor), do: {:error, :invalid_cursor}

  defp encode_cursor(%Message{inserted_at: inserted_at, id: id}) do
    "#{DateTime.to_unix(inserted_at, :microsecond)}:#{id}"
    |> Base.url_encode64(padding: false)
  end

  defp decode_cursor(cursor) do
    with {:ok, raw} <- Base.url_decode64(cursor, padding: false),
         [usec_s, id] <- String.split(raw, ":", parts: 2),
         {usec, ""} <- Integer.parse(usec_s),
         {:ok, inserted_at} <- DateTime.from_unix(usec, :microsecond) do
      {:ok, {inserted_at, id}}
    else
      _ -> :invalid
    end
  end
end
