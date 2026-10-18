# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb do
  @moduledoc "Web-layer macros for channels, sockets, controllers and the router."

  def channel do
    quote do
      use Phoenix.Channel
    end
  end

  def controller do
    quote do
      use Phoenix.Controller, namespace: SutraChatWeb
      import Plug.Conn
    end
  end

  def router do
    quote do
      use Phoenix.Router, helpers: false
      import Plug.Conn
      import Phoenix.Controller
    end
  end

  defmacro __using__(which) when is_atom(which) do
    apply(__MODULE__, which, [])
  end
end
