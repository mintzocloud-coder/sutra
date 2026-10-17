# SPDX-License-Identifier: AGPL-3.0-or-later
ExUnit.start()
# `mix test` boots the application before this file runs, so the Repo is
# up; manual mode means each test explicitly checks out a DB connection
# (via SutraChat.DataCase) and rolls back afterwards.
Ecto.Adapters.SQL.Sandbox.mode(SutraChat.Repo, :manual)
