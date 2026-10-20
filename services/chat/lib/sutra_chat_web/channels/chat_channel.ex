# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.ChatChannel do
  @moduledoc """
  Realtime chat channel with PostgreSQL-backed persistence.

  Send path is persist-then-broadcast-then-ack: a message is durably stored
  (with `client_msg_id` idempotency) before any subscriber sees it, so a
  crash between broadcast and persist cannot lose a message that clients
  believe was sent. After both persist and broadcast complete, the sender —
  and only the sender — receives a `msg:ack` push event carrying the
  server-assigned id, the echoed `client_msg_id`, a `deduped` flag (true when
  this send was a retry of an already-stored message), and the server
  timestamp. The ack is a pushed event rather than the push reply, so there
  is exactly one confirmation path and it can never precede durability.
  Delivery semantics are at-least-once; see `docs/delivery-semantics.md`.
  Joins return recent history, cursor-paged.

  Presence: every join tracks the subject with a per-device meta
  (`online_at` ms, `device`, `status`) and pushes `"presence_state"`;
  joins/leaves broadcast as `"presence_diff"` (emitted by
  `Phoenix.Presence` itself). The full contract lives in
  `SutraChat.Presence` and `docs/presence.md`.

  Authorization: only authenticated sockets may join (enforced first at
  `UserSocket.connect/3`, checked again here via
  `SutraChat.Chat.RoomPolicy`); Wave 1 allows any authenticated subject
  into any well-formed `room:*` topic — see `docs/room-membership.md` for
  the Wave 1 → Wave 2 membership roadmap. Send payloads are size-capped
  (body 4096 chars, client_msg_id 128 chars) before reaching persistence.

  E2E session establishment (I2): the `e2e:session_init` event carries
  the initiator's X3DH initial message (computed on-device — the server
  performs no crypto). The envelope is validated by
  `SutraChat.E2E.Handshake`, persisted with kind `"e2e_session_init"`
  before broadcast (so offline recipients replay it from history), and
  broadcast as `e2e:session_init`. Clients decode `body` as JSON when
  `kind == "e2e_session_init"`. Full protocol contract: `docs/e2e.md`.

  E2E messages (I3): after the session is established, ratchet messages
  travel as ciphertext inside an `e2e_data` envelope pushed with the
  `message:new` event (validated by `SutraChat.E2E.Envelope`, persisted
  with kind `"e2e_message"` — ciphertext only, never plaintext — then
  broadcast as `message:new` and acked like a normal send). A send is
  either plaintext (`body`) or E2E (`e2e_data`), never both. The first
  persisted `e2e:session_init` in a room flips the per-conversation E2E
  flag (served on join as `e2e_enabled` and via the Go API's
  `GET /v1/conversations/{room_id}/e2e`); clients must never claim E2E
  on a conversation where the flag is not set.
  """
  use SutraChatWeb, :channel

  require Logger

  alias SutraChat.Chat
  alias SutraChat.Chat.RoomPolicy
  alias SutraChat.E2E
  alias SutraChat.E2E.Envelope
  alias SutraChat.E2E.Handshake
  alias SutraChat.Presence

  @impl true
  def join("room:" <> room_id, params, socket) do
    case RoomPolicy.authorize_join(socket.assigns[:user_id], room_id) do
      {:error, :unauthorized} ->
        # Should not happen: UserSocket.connect/3 refuses unauthenticated
        # connects. This is defense-in-depth for any future entry point
        # that could hand the channel a socket without an identity.
        {:error, %{reason: "unauthorized"}}

      {:error, :invalid_room_id} ->
        {:error, %{reason: "invalid room id"}}

      :ok ->
        send(self(), :after_join)
        socket = assign(socket, :room_id, room_id)
        # Stable per-install device id lets the presence roster label
        # "phone" vs "laptop"; absent/invalid → server mints a per-session
        # id in :after_join so metas still distinguish devices.
        socket = assign(socket, :device_id, join_device_id(params))

        case Chat.history(room_id, history_limit(params), history_cursor(params)) do
          {:ok, page} ->
            {:ok,
             %{
               room_id: room_id,
               history: render_page(page),
               # I3: evidence-based flag — true only once the server has
               # persisted an e2e:session_init in this room. Clients must
               # never display E2E UI when this is false.
               e2e_enabled: E2E.conversation_e2e_enabled?(room_id)
             }, socket}

          {:error, :invalid_cursor} ->
            {:error, %{reason: "invalid history cursor"}}
        end
    end
  end

  @impl true
  def handle_info(:after_join, socket) do
    # One meta per channel join (one per tracking pid): the same user on
    # two devices accumulates two metas under their user_id key.
    # "presence_diff" broadcasts on track/untrack are emitted by
    # Phoenix.Presence itself — see SutraChat.Presence for the contract.
    {:ok, _} =
      Presence.track(socket, socket.assigns.user_id, %{
        online_at: System.system_time(:millisecond),
        device: device_id(socket),
        status: "online"
      })

    push(socket, "presence_state", Presence.list(socket))
    {:noreply, socket}
  end

  # Client-supplied stable id (per app install); capped like client_msg_id.
  defp device_id(socket) do
    case socket.assigns[:device_id] do
      id when is_binary(id) and byte_size(id) > 0 -> id
      _ -> "anon-" <> Base.encode16(:crypto.strong_rand_bytes(6), case: :lower)
    end
  end

  defp join_device_id(%{"device_id" => id})
       when is_binary(id) and byte_size(id) > 0 and byte_size(id) <= 128,
       do: id

  defp join_device_id(_params), do: nil

  # client_msg_id is transport-level idempotency: required and capped
  # exactly like the plaintext path (RoomPolicy.authorize_send's rules).
  defp valid_client_msg_id?(id),
    do: is_binary(id) and id != "" and byte_size(id) <= 128

  defp envelope_error(:missing_field), do: "e2e envelope missing a required field"
  defp envelope_error(:invalid_payload), do: "invalid e2e envelope"
  defp envelope_error(:field_too_long), do: "e2e envelope field too long"
  defp envelope_error(:bad_base64), do: "e2e envelope key field is not valid base64"
  defp envelope_error(:bad_key_size), do: "e2e ratchet key must be 32 bytes"
  defp envelope_error(:bad_counter), do: "e2e envelope counters must be non-negative integers"
  defp envelope_error(:ciphertext_empty), do: "e2e ciphertext must not be empty"
  defp envelope_error(:ciphertext_too_large), do: "e2e ciphertext too large"

  @impl true
  def handle_in("message:new", %{"e2e_data" => _} = params, socket) do
    client_msg_id = params["client_msg_id"]

    cond do
      Map.has_key?(params, "body") ->
        {:reply, {:error, %{reason: "a message is either plaintext or E2E, never both"}},
         socket}

      not valid_client_msg_id?(client_msg_id) ->
        {:reply, {:error, %{reason: "client_msg_id required", max_length: 128}}, socket}

      true ->
        case Envelope.validate_message(params["e2e_data"]) do
          {:ok, envelope} ->
            persist_and_broadcast(socket, Jason.encode!(envelope), client_msg_id, "e2e_message")

          {:error, reason} ->
            {:reply, {:error, %{reason: envelope_error(reason)}}, socket}
        end
    end
  end

  @impl true
  def handle_in("message:new", %{"body" => body, "client_msg_id" => client_msg_id}, socket) do
    case RoomPolicy.authorize_send(body, client_msg_id) do
      :ok ->
        persist_and_broadcast(socket, body, client_msg_id)

      {:error, :invalid_payload} ->
        {:reply, {:error, %{reason: "body and client_msg_id required"}}, socket}

      {:error, :client_msg_id_too_long} ->
        {:reply, {:error, %{reason: "client_msg_id too long", max_length: 128}}, socket}

      {:error, :body_too_long} ->
        {:reply, {:error, %{reason: "message too long", max_length: 4_096}}, socket}
    end
  end

  def handle_in("message:new", _params, socket) do
    {:reply, {:error, %{reason: "body and client_msg_id required"}}, socket}
  end

  # I2: X3DH session-establishment transport. The initiator computed the
  # X3DH on-device (the server performs NO crypto); this event carries
  # the opaque PreKey-style initial message to the recipient. The
  # envelope is validated (shape, base64, exact key sizes, ciphertext
  # cap), persisted BEFORE broadcast with kind "e2e_session_init" — an
  # offline recipient replays it from history, which is required because
  # their first ratchet step is derived from exactly this message — then
  # broadcast as "e2e:session_init" and acked to the sender like a normal
  # send (idempotent on (sender_subject, client_msg_id): a retry never
  # establishes a second session).
  @impl true
  def handle_in("e2e:session_init", params, socket) do
    case Handshake.validate_init(params) do
      {:ok, envelope} ->
        init_and_broadcast(socket, envelope)

      {:error, reason} ->
        {:reply, {:error, %{reason: handshake_error(reason)}}, socket}
    end
  end

  # -- persist path ----------------------------------------------------

  defp persist_and_broadcast(socket, body, client_msg_id) do
    persist_and_broadcast(socket, body, client_msg_id, "text")
  end

  defp persist_and_broadcast(socket, body, client_msg_id, kind) do
    attrs = %{
      room_id: socket.assigns.room_id,
      sender_subject: socket.assigns.user_id,
      client_msg_id: client_msg_id,
      body: body,
      kind: kind
    }

    case Chat.persist_message(attrs) do
      {:ok, message, deduped} ->
        # I3: the first persisted session-init is the evidence that X3DH
        # establishment happened in this room — flip the per-conversation
        # E2E flag (best-effort: the flag is a convenience cache, the
        # persisted init row is the real evidence, so a write failure is
        # logged, never fatal to the send).
        if kind == "e2e_session_init" and not deduped do
          case E2E.mark_conversation_e2e(socket.assigns.room_id, message.id) do
            :ok -> :ok
            {:error, reason} -> Logger.warning("e2e: flag flip failed", room_id: socket.assigns.room_id, reason: inspect(reason))
          end
        end

        # Broadcast only after the write is durable: what clients see as
        # "sent" always survives a restart. Handshake messages broadcast
        # under their own event name so recipients can pick them out.
        event = if kind == "e2e_session_init", do: "e2e:session_init", else: "message:new"
        broadcast!(socket, event, render_message(message))
        # The ack is a pushed event (not the push reply) so there is exactly
        # one confirmation path, and it fires only after BOTH persist and
        # broadcast completed — no ack ever precedes durability.
        push(socket, "msg:ack", Chat.ack_payload(message, deduped))
        {:noreply, socket}

      {:error, changeset} ->
        {:reply, {:error, %{reason: "invalid message", errors: errors_on(changeset)}}, socket}
    end
  end

  # The stored body is the canonical JSON envelope: ciphertext +
  # public keys only, never plaintext. Clients Jason-decode `body` when
  # `kind == "e2e_session_init"`.
  defp init_and_broadcast(socket, envelope) do
    persist_and_broadcast(
      socket,
      Jason.encode!(envelope),
      envelope["client_msg_id"],
      "e2e_session_init"
    )
  end

  defp handshake_error(:missing_field), do: "handshake envelope missing a required field"
  defp handshake_error(:invalid_payload), do: "invalid handshake envelope"
  defp handshake_error(:field_too_long), do: "handshake field too long"
  defp handshake_error(:bad_base64), do: "handshake key field is not valid base64"
  defp handshake_error(:bad_key_size), do: "handshake public key must be 32 bytes"
  defp handshake_error(:bad_registration_id), do: "registration_id must be 1..65535"
  defp handshake_error(:bad_prekey_id), do: "prekey id must be 1..16777215"
  defp handshake_error(:ciphertext_empty), do: "handshake ciphertext must not be empty"
  defp handshake_error(:ciphertext_too_large), do: "handshake ciphertext too large"

  # -- history params -------------------------------------------------

  defp history_limit(%{"limit" => limit}) when is_binary(limit) do
    case Integer.parse(limit) do
      {n, ""} -> n
      _ -> 50
    end
  end

  defp history_limit(%{"limit" => limit}) when is_integer(limit), do: limit
  defp history_limit(_params), do: 50

  defp history_cursor(%{"cursor" => cursor}) when is_binary(cursor), do: cursor
  defp history_cursor(_params), do: nil

  # -- rendering ------------------------------------------------------

  defp render_page(%{messages: messages, next_cursor: next_cursor}) do
    %{messages: Enum.map(messages, &render_message/1), next_cursor: next_cursor}
  end

  defp render_message(message) do
    %{
      id: message.id,
      room_id: message.room_id,
      body: message.body,
      kind: message.kind,
      client_msg_id: message.client_msg_id,
      user_id: message.sender_subject,
      sent_at: DateTime.to_unix(message.inserted_at, :millisecond)
    }
  end

  defp errors_on(changeset) do
    Ecto.Changeset.traverse_errors(changeset, fn {msg, opts} ->
      Enum.reduce(opts, msg, fn {key, value}, acc ->
        String.replace(acc, "%{#{key}}", to_string(value))
      end)
    end)
  end
end
