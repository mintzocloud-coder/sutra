# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.UserSocket do
  @moduledoc """
  Authenticated websocket entry point.

  Clients connect with their OIDC access token as the `token` connect
  param:

      socket = new Socket("/socket", {params: {token: "<access token>"}})

  The token is verified against the issuer's JWKS under the same contract
  the Go API service enforces (RS256, expiry, issuer, audience — see
  `docs/auth-contract.md`). A refused connect never reaches a channel;
  a verified connect assigns the token's subject as the socket identity,
  which channels use for `sender_subject` and presence tracking.
  """
  use Phoenix.Socket

  require Logger

  alias SutraChat.Auth.TokenVerifier

  channel "room:*", SutraChatWeb.ChatChannel

  @impl true
  def connect(%{"token" => token}, socket, _connect_info) when is_binary(token) do
    case TokenVerifier.verify(String.trim(token)) do
      {:ok, claims} ->
        {:ok, assign(socket, :user_id, claims.subject)}

      {:error, reason} ->
        Logger.warning("websocket connect refused: #{inspect(reason)}")
        :error
    end
  end

  # No token at all: refuse. There is no anonymous access to channels.
  def connect(_params, _socket, _connect_info), do: :error

  @impl true
  def id(socket), do: "user_socket:#{socket.assigns.user_id}"
end
