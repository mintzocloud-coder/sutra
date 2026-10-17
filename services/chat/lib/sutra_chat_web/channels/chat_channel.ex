# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.ChatChannel do
  @moduledoc """
  Realtime chat channel with PostgreSQL-backed persistence.

  Send path is persist-then-broadcast: a message is durably stored (with
  `client_msg_id` idempotency) before any subscriber sees it, so a crash
  between broadcast and persist cannot lose a message that clients believe
  was sent. Joins return recent history, cursor-paged.

  Authorization: only authenticated sockets may join (enforced first at
  `UserSocket.connect/3`, checked again here via
  `SutraChat.Chat.RoomPolicy`); Wave 1 allows any authenticated subject
  into any well-formed `room:*` topic — see `docs/room-membership.md` for
  the Wave 1 → Wave 2 membership roadmap. Send payloads are size-capped
  (body 4096 chars, client_msg_id 128 chars) before reaching persistence.
  """
  use SutraChatWeb, :channel

  alias SutraChat.Chat
  alias SutraChat.Chat.RoomPolicy
  alias SutraChat.Presence

  @impl true
  def join("room:" <> room_id, params, socket) do
    case RoomPolicy.authorize_join(socket.assigns[:user_id], room_id) do
      {:error, :unauthorized} ->
        # Should not happen: UserSocket.connect/3 refuses unauthenticated
        # connects. This is defense-in-depth for any future entry point
        # that could hand the channel a socket without an identity.
        {:error, %{reason: "unauthorized"}}

      {:error, :invalid_room_id} ->
        {:error, %{reason: "invalid room id"}}

      :ok ->
        send(self(), :after_join)
        socket = assign(socket, :room_id, room_id)

        case Chat.history(room_id, history_limit(params), history_cursor(params)) do
          {:ok, page} ->
            {:ok, %{room_id: room_id, history: render_page(page)}, socket}

          {:error, :invalid_cursor} ->
            {:error, %{reason: "invalid history cursor"}}
        end
    end
  end

  @impl true
  def handle_info(:after_join, socket) do
    {:ok, _} =
      Presence.track(socket, socket.assigns.user_id, %{
        online_at: System.system_time(:second)
      })

    push(socket, "presence_state", Presence.list(socket))
    {:noreply, socket}
  end

  @impl true
  def handle_in("message:new", %{"body" => body, "client_msg_id" => client_msg_id}, socket) do
    case RoomPolicy.authorize_send(body, client_msg_id) do
      :ok ->
        persist_and_broadcast(socket, body, client_msg_id)

      {:error, :invalid_payload} ->
        {:reply, {:error, %{reason: "body and client_msg_id required"}}, socket}

      {:error, :client_msg_id_too_long} ->
        {:reply, {:error, %{reason: "client_msg_id too long", max_length: 128}}, socket}

      {:error, :body_too_long} ->
        {:reply, {:error, %{reason: "message too long", max_length: 4_096}}, socket}
    end
  end

  def handle_in("message:new", _params, socket) do
    {:reply, {:error, %{reason: "body and client_msg_id required"}}, socket}
  end

  # -- persist path ----------------------------------------------------

  defp persist_and_broadcast(socket, body, client_msg_id) do
    attrs = %{
      room_id: socket.assigns.room_id,
      sender_subject: socket.assigns.user_id,
      client_msg_id: client_msg_id,
      body: body
    }

    case Chat.persist_message(attrs) do
      {:ok, message} ->
        # Broadcast only after the write is durable: what clients see as
        # "sent" always survives a restart.
        broadcast!(socket, "message:new", render_message(message))
        {:reply, {:ok, %{id: message.id, client_msg_id: message.client_msg_id}}, socket}

      {:error, changeset} ->
        {:reply, {:error, %{reason: "invalid message", errors: errors_on(changeset)}}, socket}
    end
  end

  # -- history params -------------------------------------------------

  defp history_limit(%{"limit" => limit}) when is_binary(limit) do
    case Integer.parse(limit) do
      {n, ""} -> n
      _ -> 50
    end
  end

  defp history_limit(%{"limit" => limit}) when is_integer(limit), do: limit
  defp history_limit(_params), do: 50

  defp history_cursor(%{"cursor" => cursor}) when is_binary(cursor), do: cursor
  defp history_cursor(_params), do: nil

  # -- rendering ------------------------------------------------------

  defp render_page(%{messages: messages, next_cursor: next_cursor}) do
    %{messages: Enum.map(messages, &render_message/1), next_cursor: next_cursor}
  end

  defp render_message(message) do
    %{
      id: message.id,
      room_id: message.room_id,
      body: message.body,
      client_msg_id: message.client_msg_id,
      user_id: message.sender_subject,
      sent_at: DateTime.to_unix(message.inserted_at, :millisecond)
    }
  end

  defp errors_on(changeset) do
    Ecto.Changeset.traverse_errors(changeset, fn {msg, opts} ->
      Enum.reduce(opts, msg, fn {key, value}, acc ->
        String.replace(acc, "%{#{key}}", to_string(value))
      end)
    end)
  end
end
