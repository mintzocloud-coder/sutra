# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Repo.Migrations.AddE2EMessageKind do
  @moduledoc false
  use Ecto.Migration

  # I3: ratchet-message envelopes travel over `message:new` and persist
  # with kind "e2e_message" (ciphertext only, never plaintext). The CHECK
  # is dropped and re-created — Postgres cannot alter a CHECK in place.
  def change do
    drop(constraint(:messages, :kind_must_be_known))

    create(
      constraint(:messages, :kind_must_be_known,
        check: "kind IN ('text', 'e2e_session_init', 'e2e_message')"
      )
    )
  end
end
