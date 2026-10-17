# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.ChatChannel do
  @moduledoc """
  Realtime chat channel. First slice: join, broadcast, presence.
  Persistence (PG18 append-only store with client_msg_id idempotency)
  is the follow-up issue.
  """
  use SutraChatWeb, :channel

  alias SutraChat.Presence

  @impl true
  def join("room:" <> room_id, _params, socket) do
    send(self(), :after_join)
    {:ok, %{room_id: room_id}, assign(socket, :room_id, room_id)}
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
    # TODO: persist to PG18 with client_msg_id idempotency BEFORE broadcast.
    broadcast!(socket, "message:new", %{
      body: body,
      client_msg_id: client_msg_id,
      user_id: socket.assigns.user_id,
      sent_at: System.system_time(:millisecond)
    })

    {:reply, {:ok, %{client_msg_id: client_msg_id}}, socket}
  end

  def handle_in("message:new", _params, socket) do
    {:reply, {:error, %{reason: "body and client_msg_id required"}}, socket}
  end
end
