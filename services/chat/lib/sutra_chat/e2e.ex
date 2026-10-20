# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.E2E do
  @moduledoc """
  Chat-side E2E device-key directory reads (I1, part 2b).

  The `e2e_devices` / `e2e_one_time_prekeys` / `users` tables are OWNED by
  the Go API service (migrations 0021 and 0002, `schema_migrations` ledger)
  on the shared database. This context performs the chat side's only
  key-directory operation — the atomic one-time-prekey consume that X3DH
  session initiation (I2) needs. No Elixir migration touches these tables;
  the DDL source of truth stays with the Go migrations.

  `fetch_prekey_bundle/2` mirrors the Go API's `ConsumePrekey` semantics
  exactly:

    * one transaction: device lookup (joined on the caller's OIDC subject,
      so no UUID plumbing crosses the boundary), lowest-id unconsumed
      prekey consumed via `SELECT ... FOR UPDATE SKIP LOCKED` — racing
      initiators can never receive the same prekey, which is the property
      the first message's forward secrecy rests on;
    * queue dry → bundle still returns with `one_time_prekey: nil`; the
      initiator falls back to signed-prekey-only X3DH (still
      authenticated, still forward-secret from the ratchet onward);
    * unknown subject or device → `{:error, :not_found}` — no existence
      oracle beyond what the API's own endpoints already expose.

  Key material leaves here base64-encoded (standard, padded), the same
  wire shape the Go API serves. The server never sees private keys —
  every byte in these tables is public by design (see docs/e2e.md).
  """

  alias SutraChat.Repo

  # Mirrors migration 0021's CHECK (device_id <> '' AND char_length <= 64).
  @max_device_id_length 64
  @max_subject_length 256

  @doc """
  Fetches the public prekey bundle for `target_subject`'s `device_id` and
  atomically consumes its lowest-id available one-time prekey.

  Returns `{:ok, bundle}` with string keys matching the Go API's
  PrekeyBundle JSON shape, or `{:error, :not_found}` /
  `{:error, :invalid_request}`.
  """
  @spec fetch_prekey_bundle(String.t(), String.t()) :: {:ok, map()} | {:error, atom()}
  def fetch_prekey_bundle(target_subject, device_id)
      when is_binary(target_subject) and is_binary(device_id) do
    with :ok <- validate_subject(target_subject),
         :ok <- validate_device_id(device_id) do
      Repo.transaction(fn ->
        case consume_bundle(target_subject, device_id) do
          {:ok, bundle} -> bundle
          {:error, reason} -> Repo.rollback(reason)
        end
      end)
    end
  end

  def fetch_prekey_bundle(_, _), do: {:error, :invalid_request}

  # -- validation -------------------------------------------------------

  defp validate_subject(s) when byte_size(s) in 1..@max_subject_length, do: :ok
  defp validate_subject(_), do: {:error, :invalid_request}

  defp validate_device_id(d) when byte_size(d) in 1..@max_device_id_length, do: :ok
  defp validate_device_id(_), do: {:error, :invalid_request}

  # -- consume ----------------------------------------------------------

  # Device row plus its unconsumed queue, resolved by OIDC subject.
  @device_sql """
  SELECT d.id, d.registration_id, d.identity_key, d.signed_prekey,
         d.signed_prekey_id, d.signed_prekey_sig
  FROM e2e_devices d
  JOIN users u ON u.id = d.user_id
  WHERE u.subject = $1 AND d.device_id = $2
  """

  # Backed by migration 0021's partial index
  # (device_id, prekey_id) WHERE consumed_at IS NULL.
  @consume_sql """
  SELECT id, prekey_id, public_key FROM e2e_one_time_prekeys
  WHERE device_id = $1 AND consumed_at IS NULL
  ORDER BY prekey_id LIMIT 1 FOR UPDATE SKIP LOCKED
  """

  @mark_consumed_sql """
  UPDATE e2e_one_time_prekeys SET consumed_at = now() WHERE id = $1
  """

  @available_sql """
  SELECT count(*) FROM e2e_one_time_prekeys
  WHERE device_id = $1 AND consumed_at IS NULL
  """

  defp consume_bundle(subject, device_id) do
    case Repo.query!(@device_sql, [subject, device_id]) do
      %{rows: []} ->
        {:error, :not_found}

      %{rows: [[dev_id, reg_id, ikey, spk, spk_id, spk_sig]]} ->
        # dev_id arrives as the 16-byte binary Postgrex decodes uuids
        # to — it stays in that form for the queries below, which is
        # exactly what the driver expects for uuid parameters.
        one_time = consume_one_time(dev_id)
        available = available_count(dev_id)

        {:ok,
         %{
           "device_id" => device_id,
           "registration_id" => reg_id,
           "identity_key" => Base.encode64(ikey),
           "signed_prekey" => Base.encode64(spk),
           "signed_prekey_id" => spk_id,
           "signed_prekey_sig" => Base.encode64(spk_sig),
           "one_time_prekey" => one_time,
           "available_prekeys" => available
         }}
    end
  end

  defp consume_one_time(dev_id) do
    case Repo.query!(@consume_sql, [dev_id]) do
      %{rows: []} ->
        # Queue dry — signed-prekey-only X3DH fallback, not an error.
        nil

      %{rows: [[pk_id, pk_prekey_id, pk_bytes]]} ->
        Repo.query!(@mark_consumed_sql, [pk_id])
        %{"id" => pk_prekey_id, "public_key" => Base.encode64(pk_bytes)}
    end
  end

  defp available_count(dev_id) do
    %{rows: [[n]]} = Repo.query!(@available_sql, [dev_id])
    n
  end
end
