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

  # -- per-conversation E2E flag (I3) -------------------------------------

  # The `e2e_conversations` table is OWNED by the Go API (migration
  # 0022, `schema_migrations` ledger) on the shared database — no Elixir
  # migration touches it, same convention as the I1 device tables. The
  # chat service owns rooms in Wave 1 and produces the evidence (it
  # persists the `e2e:session_init` messages), so it is the writer; the
  # Go API exposes the flag read-only at
  # `GET /v1/conversations/{room_id}/e2e`.

  # Room ids travel in the channel topic ("room:<room_id>") — same
  # grammar as SutraChat.Chat.RoomPolicy (`^[A-Za-z0-9_-]{1,64}$`).
  @room_id_pattern ~r/\A[A-Za-z0-9_-]{1,64}\z/

  # One transaction: INSERT the room row (or touch nothing when it
  # exists) and, ONLY when the row is new or still disabled, flip
  # e2e_enabled and pin first_init_message_id to the FIRST session-init
  # message id — the evidence anchor. Once enabled the flag never flips
  # back: E2E establishment is a one-way, monotonic fact.
  @mark_e2e_sql """
  INSERT INTO e2e_conversations (room_id, e2e_enabled, enabled_at, first_init_message_id)
  VALUES ($1, true, now(), $2)
  ON CONFLICT (room_id) DO UPDATE
    SET e2e_enabled = true,
        enabled_at = COALESCE(e2e_conversations.enabled_at, now()),
        first_init_message_id = COALESCE(e2e_conversations.first_init_message_id, EXCLUDED.first_init_message_id)
    WHERE e2e_conversations.e2e_enabled = false
  """

  @flag_sql """
  SELECT e2e_enabled FROM e2e_conversations WHERE room_id = $1
  """

  @doc """
  Records that X3DH session establishment was observed in `room_id`
  (the chat persisted a valid `e2e:session_init` there).

  Idempotent and monotonic: the first call wins (its `init_message_id`
  is pinned as the evidence anchor); later calls are no-ops. Returns
  `:ok` on success, `{:error, :invalid_room_id}` on a malformed room id,
  `{:error, :unavailable}` when the write fails — the caller treats the
  flag as a convenience cache (the persisted init message is the real
  evidence) and logs a warning rather than failing the send.
  """
  @spec mark_conversation_e2e(String.t(), String.t()) :: :ok | {:error, atom()}
  def mark_conversation_e2e(room_id, init_message_id)
      when is_binary(room_id) and is_binary(init_message_id) do
    if Regex.match?(@room_id_pattern, room_id) do
      case Repo.query(@mark_e2e_sql, [room_id, init_message_id]) do
        {:ok, _} -> :ok
        {:error, _} -> {:error, :unavailable}
      end
    else
      {:error, :invalid_room_id}
    end
  end

  def mark_conversation_e2e(_, _), do: {:error, :invalid_room_id}

  @doc """
  Reads the per-conversation E2E flag for join responses.

  Returns `true` only when the flag row exists and is enabled; `false`
  on unknown rooms, malformed ids, or any read failure — a lookup
  failure must never break a join, and "unknown" is never E2E (clients
  must not claim E2E on conversations where it is not enabled).
  """
  @spec conversation_e2e_enabled?(term()) :: boolean()
  def conversation_e2e_enabled?(room_id) when is_binary(room_id) do
    with true <- Regex.match?(@room_id_pattern, room_id),
         {:ok, %{rows: [[true]]}} <- Repo.query(@flag_sql, [room_id]) do
      true
    else
      _ -> false
    end
  end

  def conversation_e2e_enabled?(_), do: false

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
