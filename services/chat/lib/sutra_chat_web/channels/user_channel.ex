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

  E2E directory (I1): `e2e:prekey_fetch` serves the public prekey bundle
  a session initiator needs for X3DH (I2). It lives here — not on the
  room channel — because it is a directory operation about a user and
  their device, not about a room; I2's handshake events will ride the
  chat channel. Any authenticated user may fetch any other user's
  bundle — initiating a session requires the peer's public keys,
  exactly like Signal's prekey server.
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

  @impl true
  def handle_in("e2e:prekey_fetch", %{"user_id" => target, "device_id" => device_id}, socket) do
    case SutraChat.E2E.fetch_prekey_bundle(target, device_id) do
      {:ok, bundle} ->
        {:reply, {:ok, bundle}, socket}

      {:error, :not_found} ->
        {:reply, {:error, %{reason: "not_found"}}, socket}

      {:error, :invalid_request} ->
        {:reply, {:error, %{reason: "invalid_request"}}, socket}
    end
  end

  def handle_in("e2e:prekey_fetch", _payload, socket) do
    {:reply, {:error, %{reason: "invalid_request"}}, socket}
  end
end
