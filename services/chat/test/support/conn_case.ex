# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.ConnCase do
  @moduledoc """
  Test case for HTTP controller tests (D5).

  Dispatches requests in-process through `SutraChatWeb.Endpoint` as a
  plug (`Phoenix.ConnTest`) — no network listener needed, so it works
  with the endpoint's `server: false` test config. The internal-events
  service token comes from `config/runtime.exs` (`"test-internal-token"`
  in :test, overridable via `CHAT_INTERNAL_TOKEN`).
  """
  use ExUnit.CaseTemplate

  using do
    quote do
      import Plug.Conn
      import Phoenix.ConnTest
      @endpoint SutraChatWeb.Endpoint
    end
  end

  setup _tags do
    {:ok, conn: Phoenix.ConnTest.build_conn()}
  end
end
