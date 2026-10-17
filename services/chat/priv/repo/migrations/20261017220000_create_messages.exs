# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Repo.Migrations.CreateMessages do
  @moduledoc false
  use Ecto.Migration

  def change do
    create table(:messages, primary_key: false) do
      add :id, :binary_id, primary_key: true
      add :room_id, :string, null: false
      add :sender_subject, :string, null: false
      # Client-generated idempotency key; unique per sender so a retried send
      # can never create a second row.
      add :client_msg_id, :string, null: false
      add :body, :text, null: false

      timestamps(type: :utc_datetime_usec, updated_at: false)
    end

    create unique_index(:messages, [:sender_subject, :client_msg_id])
    # Backs the cursor-paged history query: room + (inserted_at, id) ordering.
    create index(:messages, [:room_id, :inserted_at, :id])
  end
end
