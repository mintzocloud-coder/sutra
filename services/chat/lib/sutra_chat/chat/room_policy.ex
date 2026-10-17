# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Chat.RoomPolicy do
  @moduledoc """
  Wave 1 room authorization policy — pure, no side effects.

  **Wave 1 model (this release): open rooms by ID.** Any authenticated
  subject (see `docs/auth-contract.md` — the socket connect is refused
  without a valid OIDC token) may join any well-formed `room:*` topic and
  publish to it. Rooms are created implicitly on first join; there are no
  invites, no roles, no per-room ACLs yet. This matches Telegram/Discord
  "join by link" pragmatics and is the documented stepping stone to:

  **Wave 2 model (planned): membership lists.** A `room_members` table
  (room_id, subject, role) with owner/member roles, explicit join/invite
  flow, and per-message authorization against the membership list.
  `authorize_join/2` keeps its shape — only its body gets stricter — so
  channel code won't churn when Wave 2 lands.

  Kept pure (no Repo, no Phoenix) so the policy is unit-testable without
  a database or a compiled Phoenix.
  """

  @typedoc "Join failure reasons surfaced to clients as `reason` strings."
  @type join_error :: :unauthorized | :invalid_room_id

  @typedoc "Send-payload failure reasons."
  @type send_error :: :invalid_payload | :client_msg_id_too_long | :body_too_long

  # Room ids travel in the channel topic ("room:<room_id>") and are
  # interpolated into queries via Ecto bindings, but they also appear in
  # logs and URLs. Keep them short and free of control/whitespace chars.
  @room_id_pattern ~r/\A[A-Za-z0-9_-]{1,64}\z/

  # Telegram caps messages at 4096 chars; matching it keeps behavior
  # unsurprising for clients written against either platform.
  @max_body_length 4_096
  @max_client_msg_id_length 128

  @doc """
  Authorize a channel join.

  The socket identity comes from `UserSocket.connect/3`, which refuses
  unauthenticated connects outright — this check is defense-in-depth for
  any future channel entry point (e.g. an authenticated REST join) that
  could hand the channel a socket without a `user_id` assign.
  """
  @spec authorize_join(String.t() | nil, String.t()) :: :ok | {:error, join_error()}
  def authorize_join(nil, _room_id), do: {:error, :unauthorized}
  def authorize_join("", _room_id), do: {:error, :unauthorized}

  def authorize_join(user_id, room_id) when is_binary(user_id) and is_binary(room_id) do
    if Regex.match?(@room_id_pattern, room_id) do
      :ok
    else
      {:error, :invalid_room_id}
    end
  end

  def authorize_join(_user_id, _room_id), do: {:error, :unauthorized}

  @doc """
  Validate a send payload's sizes before it reaches persistence.

  The `Message` changeset enforces the same caps (defense in depth), but
  rejecting up front in the channel gives clients a clean
  `{:error, %{reason: ...}}` reply instead of an Ecto error tuple.
  """
  @spec authorize_send(term(), term()) :: :ok | {:error, send_error()}
  def authorize_send(body, client_msg_id)
      when is_binary(body) and is_binary(client_msg_id) and body != "" and
             client_msg_id != "" do
    cond do
      String.length(client_msg_id) > @max_client_msg_id_length ->
        {:error, :client_msg_id_too_long}

      String.length(body) > @max_body_length ->
        {:error, :body_too_long}

      true ->
        :ok
    end
  end

  def authorize_send(_body, _client_msg_id), do: {:error, :invalid_payload}

  @doc "Documented payload limits, for tests and clients."
  @spec limits() :: %{body: pos_integer(), client_msg_id: pos_integer(), room_id: pos_integer()}
  def limits do
    %{
      body: @max_body_length,
      client_msg_id: @max_client_msg_id_length,
      room_id: 64
    }
  end
end
