# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.UserSocket do
  use Phoenix.Socket

  channel "room:*", SutraChatWeb.ChatChannel

  @impl true
  def connect(_params, socket, _connect_info) do
    # TODO(#5): verify OIDC token via Hydra JWKS; assign real user_id.
    {:ok, assign(socket, :user_id, "stub-user")}
  end

  @impl true
  def id(socket), do: "user_socket:#{socket.assigns.user_id}"
end
