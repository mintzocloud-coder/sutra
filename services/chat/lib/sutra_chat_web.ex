# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb do
  @moduledoc "Web-layer macros for channels and sockets."

  def channel do
    quote do
      use Phoenix.Channel
    end
  end

  defmacro __using__(which) when is_atom(which) do
    apply(__MODULE__, which, [])
  end
end
