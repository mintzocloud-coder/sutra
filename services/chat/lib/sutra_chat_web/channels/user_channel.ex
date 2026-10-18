# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.UserChannel do
  @moduledoc """
  Per-user event topic (D5): `user:<subject>`.

  The Go API publishes social events (`post:new`, `follower:new`) to the
  `/internal/events` HTTP endpoint; they are broadcast on these topics
  for the currently-connected sockets of that user.

  Join auth is the whole point of this channel: a socket may join ONLY
  the topic matching its own authenticated subject
  (`UserSocket.assigns.user_id`) — a user can never subscribe to someone
  else's events. Defense-in-depth behind `UserSocket.connect/3`, which
  already refuses unauthenticated connects.

  No presence tracking here (presence lives on `room:*` via
  `ChatChannel`) and no persistence — Wave 1 events are hints, the home
  timeline is the source of truth (docs/realtime-events.md).
  """
  use SutraChatWeb, :channel

  @impl true
  def join("user:" <> id, _params, socket) do
    cond do
      id == "" or String.contains?(id, ":") ->
        {:error, %{reason: "invalid topic"}}

      socket.assigns[:user_id] == id ->
        {:ok, socket}

      true ->
        {:error, %{reason: "unauthorized"}}
    end
  end
end
