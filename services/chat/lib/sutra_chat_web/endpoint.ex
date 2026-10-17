# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.Endpoint do
  use Phoenix.Endpoint, otp_app: :sutra_chat

  socket "/socket", SutraChatWeb.UserSocket,
    websocket: true,
    longpoll: false

  plug Plug.RequestId
  plug Plug.Logger
end
