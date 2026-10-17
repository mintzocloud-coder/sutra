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
  unchanged — the caller cannot distinguish "first send" from "retry", which
  is exactly the point: retries are safe by construction.

  Returns `{:ok, message}` or `{:error, changeset}`.
  """
  @spec persist_message(map()) :: {:ok, Message.t()} | {:error, Ecto.Changeset.t()}
  def persist_message(attrs) do
    %Message{}
    |> Message.changeset(attrs)
    |> Repo.insert(
      # No-op update on conflict: every column except the identity fields is
      # overwritten with the identical values being inserted, and
      # `returning: true` hands back the existing row.
      on_conflict: {:replace_all_except, [:id, :inserted_at]},
      conflict_target: [:sender_subject, :client_msg_id],
      returning: true
    )
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
