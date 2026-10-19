# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.InternalEventsController do
  @moduledoc """
  Service-to-service event ingress (D5).

  The Go API POSTs social event envelopes here — the contract is
  `docs/realtime-events.md` — and the chat fans each event out on the
  listed `user:<subject>` Phoenix topics, where `SutraChatWeb.UserChannel`
  delivers them to the currently-connected sockets of that user only.

  Auth is a shared Bearer token (`CHAT_INTERNAL_TOKEN`) compared in
  constant time via `Plug.Crypto.secure_compare/2`. An unset or empty
  token fails closed: every request 401s, so a chat without the secret
  can never accept (or silently swallow) events from an API that has
  one — both sides must agree on the secret.

  Errors use the same JSON error envelope as the Go API
  (`{"error":{"code","message","request_id"}}`); the request id comes
  from the endpoint's `Plug.RequestId`.
  """
  use SutraChatWeb, :controller

  @known_events ~w(post:new follower:new shipment:update live:started live:ended)
  @topic_re ~r/\Auser:[^:]+\z/

  # post:new payload: everything a client needs to render the
  # notification without a follow-up fetch.
  @post_new_required ~w(post_id author_id text created_at)
  @follower_new_required ~w(follower_id created_at)
  # shipment:update payload: cod_expected_paise is validated separately
  # (integer, not string — see check_shipment_money/1), everything else
  # must be a string. The chat never needs the numeric field to render;
  # the API guarantees it is an integer ≥ 0 on the wire.
  @shipment_update_required ~w(shipment_id order_id status updated_at)
  # live:started / live:ended payload (G1): enough to render "you are on
  # air" / "stream ended" on the host's own devices. ended_at is nil on
  # live:started (optional key), so it is not in the required list.
  @live_status_required ~w(room_id room_name title kind host_id started_at)

  def create(conn, _params) do
    with :ok <- check_auth(conn),
         {:ok, envelope} <- parse_envelope(conn.body_params) do
      for topic <- envelope.topics do
        :ok = SutraChatWeb.Endpoint.broadcast(topic, envelope.event, envelope.payload)
      end

      conn
      |> put_status(:accepted)
      |> json(%{status: "accepted", event: envelope.event, topics: length(envelope.topics)})
    else
      {:error, :unauthorized} ->
        error(conn, 401, "unauthorized", "missing or invalid service token")

      {:error, code, message} ->
        error(conn, 422, code, message)
    end
  end

  # -- auth -----------------------------------------------------------

  defp check_auth(conn) do
    expected =
      Application.get_env(:sutra_chat, :internal_events, [])
      |> Keyword.get(:token)

    with ["Bearer " <> provided] <- get_req_header(conn, "authorization"),
         true <- is_binary(expected) and byte_size(expected) > 0,
         true <- is_binary(provided) and byte_size(provided) > 0,
         true <- Plug.Crypto.secure_compare(provided, expected) do
      :ok
    else
      _ -> {:error, :unauthorized}
    end
  end

  # -- envelope validation --------------------------------------------

  defp parse_envelope(%{
         "topics" => topics,
         "event" => event,
         "dedupe_key" => dedupe_key,
         "payload" => payload
       })
       when is_list(topics) and is_map(payload) do
    with :ok <- check_topics(topics),
         :ok <- check_event(event),
         :ok <- check_dedupe_key(dedupe_key),
         {:ok, clean_payload} <- check_payload(event, payload) do
      {:ok, %{topics: topics, event: event, payload: clean_payload}}
    end
  end

  defp parse_envelope(_),
    do: {:error, "invalid_envelope", "topics, event, dedupe_key and payload are required"}

  defp check_topics([]),
    do: {:error, "invalid_topics", "topics must not be empty"}

  defp check_topics(topics) do
    if Enum.all?(topics, &(is_binary(&1) and &1 =~ @topic_re)) do
      :ok
    else
      {:error, "invalid_topic", "every topic must match user:<subject>"}
    end
  end

  defp check_event(event) when event in @known_events, do: :ok

  defp check_event(_),
    do: {:error, "unknown_event", "event must be one of: #{Enum.join(@known_events, ", ")}"}

  defp check_dedupe_key(key) when is_binary(key) and byte_size(key) > 0, do: :ok
  defp check_dedupe_key(_), do: {:error, "missing_dedupe_key", "dedupe_key is required"}

  defp check_payload("post:new", payload) do
    with {:ok, payload} <- check_required(payload, @post_new_required) do
      case Map.get(payload, "media_ids") do
        nil ->
          {:ok, Map.put(payload, "media_ids", [])}

        ids when is_list(ids) ->
          if Enum.all?(ids, &is_binary/1),
            do: {:ok, payload},
            else: {:error, "invalid_payload", "media_ids must be a list of strings"}

        _ ->
          {:error, "invalid_payload", "media_ids must be a list of strings"}
      end
    end
  end

  defp check_payload("follower:new", payload),
    do: check_required(payload, @follower_new_required)

  defp check_payload("shipment:update", payload) do
    with {:ok, payload} <- check_required(payload, @shipment_update_required),
         :ok <- check_shipment_money(payload) do
      {:ok, payload}
    end
  end

  defp check_payload("live:started", payload),
    do: check_required(payload, @live_status_required)

  defp check_payload("live:ended", payload),
    do: check_required(payload, @live_status_required)

  defp check_shipment_money(%{"cod_expected_paise" => paise})
       when is_integer(paise) and paise >= 0,
       do: :ok

  defp check_shipment_money(_),
    do: {:error, "invalid_payload", "cod_expected_paise must be a non-negative integer"}

  defp check_required(payload, keys) do
    missing = Enum.reject(keys, &is_binary(Map.get(payload, &1)))

    if missing == [] do
      {:ok, payload}
    else
      {:error, "invalid_payload", "payload is missing required fields: #{Enum.join(missing, ", ")}"}
    end
  end

  # -- error envelope --------------------------------------------------

  defp error(conn, status, code, message) do
    request_id = conn |> get_resp_header("x-request-id") |> List.first()

    conn
    |> put_status(status)
    |> json(%{"error" => %{"code" => code, "message" => message, "request_id" => request_id}})
  end
end
