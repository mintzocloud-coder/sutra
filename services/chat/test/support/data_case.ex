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

  @doc """
  Creates minimal local copies of the Go-owned E2E tables (`users`,
  `e2e_devices`, `e2e_one_time_prekeys`) for tests. Production DDL is
  migration 0021 (`services/api/internal/db/migrations/`); these test
  tables carry only the columns the `SutraChat.E2E` context queries —
  no foreign keys, triggers, or CHECKs, so the test DB needs neither
  pgcrypto nor the Go migration runner.
  """
  def create_e2e_tables! do
    SutraChat.Repo.query!("""
CREATE TABLE IF NOT EXISTS users (
  id uuid PRIMARY KEY,
  subject text UNIQUE NOT NULL
)
""")

    SutraChat.Repo.query!("""
CREATE TABLE IF NOT EXISTS e2e_devices (
  id uuid PRIMARY KEY,
  user_id uuid NOT NULL,
  device_id text NOT NULL,
  identity_key bytea NOT NULL,
  signed_prekey bytea NOT NULL,
  signed_prekey_id integer NOT NULL,
  signed_prekey_sig bytea NOT NULL,
  registration_id integer NOT NULL
)
""")

    SutraChat.Repo.query!("""
CREATE TABLE IF NOT EXISTS e2e_one_time_prekeys (
  id uuid PRIMARY KEY,
  device_id uuid NOT NULL,
  prekey_id integer NOT NULL,
  public_key bytea NOT NULL,
  consumed_at timestamptz
)
""")

    :ok
  end

  @doc """
  Inserts a user + device + one-time prekeys into the test E2E tables.
  Returns the raw key bytes so tests can assert the base64 wire encoding
  round-trips. `prekey_ids` defaults to `[1, 2, 3]`; pass `[]` for a dry
  queue (signed-prekey-only fallback path).
  """
  def insert_e2e_device!(subject, device_id, prekey_ids \\ [1, 2, 3]) do
    {:ok, user_bin} = Ecto.UUID.dump(Ecto.UUID.generate())
    {:ok, dev_bin} = Ecto.UUID.dump(Ecto.UUID.generate())

    ikey = :crypto.strong_rand_bytes(32)
    spk = :crypto.strong_rand_bytes(32)
    sig = :crypto.strong_rand_bytes(64)

    SutraChat.Repo.query!("INSERT INTO users (id, subject) VALUES ($1, $2)", [user_bin, subject])

    SutraChat.Repo.query!(
"""
INSERT INTO e2e_devices
  (id, user_id, device_id, identity_key, signed_prekey,
   signed_prekey_id, signed_prekey_sig, registration_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
""",
      [dev_bin, user_bin, device_id, ikey, spk, 7, sig, 4242]
    )

    for pid <- prekey_ids do
      {:ok, pk_bin} = Ecto.UUID.dump(Ecto.UUID.generate())

      SutraChat.Repo.query!(
      "INSERT INTO e2e_one_time_prekeys (id, device_id, prekey_id, public_key) VALUES ($1, $2, $3, $4)",
      [pk_bin, dev_bin, pid, :crypto.strong_rand_bytes(32)]
    )
    end

    %{identity_key: ikey, signed_prekey: spk, signed_prekey_sig: sig}
  end
end
