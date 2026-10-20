# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Repo.Migrations.AddKindToMessages do
  @moduledoc false
  use Ecto.Migration

  # I2: distinguishes the X3DH session-establishment transport message
  # (`e2e_session_init`) from plain text. The server persists only the
  # opaque handshake envelope (ciphertext + public keys) — never
  # plaintext — so a single messages table stays the durability story
  # for every send path. I3 adds `e2e_data` for ratchet message
  # envelopes; plain-text `text` rows remain for non-E2E rooms.
  def change do
    alter table(:messages) do
      add :kind, :string, null: false, default: "text"
    end

    create constraint(:messages, :kind_must_be_known,
             check: "kind IN ('text', 'e2e_session_init')"
           )
  end
end
