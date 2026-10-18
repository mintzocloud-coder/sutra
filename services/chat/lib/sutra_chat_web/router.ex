# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.Router do
  @moduledoc """
  HTTP routes for the chat service (D5).

  The websocket API (UserSocket + channels) is the product surface; this
  router carries only the service-to-service internal ingress:
  `POST /internal/events`, which the Go API calls to publish social
  events for fan-out on `user:<subject>` topics. It is wired into the
  endpoint's plug pipeline and must never be exposed publicly — the
  compose file keeps it on the internal network only.
  """
  use SutraChatWeb, :router

  pipeline :internal do
    plug Plug.Parsers,
      parsers: [:json],
      pass: ["application/json"],
      json_decoder: Jason
  end

  scope "/internal", SutraChatWeb do
    pipe_through :internal
    post "/events", InternalEventsController, :create
  end
end
