# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.ChannelCase do
  @moduledoc """
  Test case for channel integration tests (B6).

  Joins channels in-process against the real endpoint (started by
  `mix test` with `server: false`), the real `SutraChat.Repo` (Ecto SQL
  sandbox — see `SutraChat.DataCase`), and the real `SutraChat.Presence`.
  Always `async: false`: channel processes run apart from the test
  process and must share its sandboxed connection.
  """
  use ExUnit.CaseTemplate

  using do
    quote do
      use Phoenix.ChannelTest
      @endpoint SutraChatWeb.Endpoint
    end
  end

  setup tags do
    SutraChat.DataCase.setup_sandbox(tags)
    :ok
  end
end
