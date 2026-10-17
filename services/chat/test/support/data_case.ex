# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.DataCase do
  @moduledoc """
  Test case for database-touching tests (B6).

  Each test checks out a connection from the Ecto SQL sandbox and rolls
  back when the test exits, so channel tests can hit the real Repo
  (persist/dedupe/history) without tests interfering with each other.
  Async tests get an isolated connection; `async: false` tests (all the
  channel tests) share one connection, which is what lets the channel
  process — spawned separately by `Phoenix.ChannelTest` — use the same
  transaction as the test.
  """
  use ExUnit.CaseTemplate

  using do
    quote do
      alias SutraChat.Repo

      import Ecto
      import Ecto.Changeset
      import Ecto.Query
      import SutraChat.DataCase
    end
  end

  setup tags do
    SutraChat.DataCase.setup_sandbox(tags)
    :ok
  end

  @doc """
  Checks out a sandbox connection for the calling test. Shared with any
  linked/spawned processes (e.g. channel processes) when the test is not
  async.
  """
  def setup_sandbox(tags) do
    pid = Ecto.Adapters.SQL.Sandbox.start_owner!(SutraChat.Repo, shared: not tags[:async])
    on_exit(fn -> Ecto.Adapters.SQL.Sandbox.stop_owner(pid) end)
  end
end
