# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Repo do
  @moduledoc """
  Ecto repository for the chat service (PostgreSQL).

  Supervised under `SutraChat.Application`; configuration comes from
  `config/config.exs` (dev/test) and `config/runtime.exs` (prod, DATABASE_URL).
  """
  use Ecto.Repo,
    otp_app: :sutra_chat,
    adapter: Ecto.Adapters.Postgres
end
