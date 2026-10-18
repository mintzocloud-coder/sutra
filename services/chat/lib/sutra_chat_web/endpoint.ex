# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.Endpoint do
  use Phoenix.Endpoint, otp_app: :sutra_chat

  socket "/socket", SutraChatWeb.UserSocket,
    websocket: true,
    longpoll: false

  plug Plug.RequestId
  plug Plug.Logger

  # D5: HTTP ingress for the service-to-service event endpoint
  # (POST /internal/events). The websocket API above is the product
  # surface; this router carries internal traffic only.
  plug SutraChatWeb.Router
end
