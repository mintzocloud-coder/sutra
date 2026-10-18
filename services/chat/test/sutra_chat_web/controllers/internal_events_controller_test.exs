# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChatWeb.InternalEventsControllerTest do
  @moduledoc """
  HTTP tests for the D5 service-to-service event ingress.

  Covers the auth boundary (missing/wrong token → 401 — the constant-time
  comparison means a wrong token can never pass, and an unset token fails
  closed), envelope validation (unknown event / bad topic / missing
  dedupe_key / bad payload → 422 with the shared error envelope), and the
  happy path: a valid envelope is accepted (202) and fanned out on the
  listed `user:<subject>` topics with the documented payload shape.
  """
  use SutraChatWeb.ConnCase, async: false

  # Must match config/runtime.exs's :test default (overridable via
  # CHAT_INTERNAL_TOKEN, which CI does not set).
  @token "test-internal-token"

  defp post_event(conn, body, token \\ @token) do
    conn =
      conn
      |> put_req_header("content-type", "application/json")
      |> put_req_header("authorization", "Bearer #{token}")

    post(conn, "/internal/events", Jason.encode!(body))
  end

  defp envelope(overrides \\ %{}) do
    Map.merge(
      %{
        "topics" => ["user:alice"],
        "event" => "post:new",
        "dedupe_key" => "post:123",
        "payload" => %{
          "post_id" => "123",
          "author_id" => "456",
          "author_handle" => "bob",
          "text" => "hello",
          "media_ids" => [],
          "created_at" => "2026-10-18T10:00:00Z"
        }
      },
      overrides
    )
  end

  # -- auth -------------------------------------------------------------

  test "rejects a missing token with 401", %{conn: conn} do
    conn =
      conn
      |> put_req_header("content-type", "application/json")
      |> post("/internal/events", Jason.encode!(envelope()))

    assert %{"error" => %{"code" => "unauthorized"}} = json_response(conn, 401)
  end

  test "rejects a wrong token with 401", %{conn: conn} do
    conn = post_event(conn, envelope(), "wrong-token")
    assert %{"error" => %{"code" => "unauthorized"}} = json_response(conn, 401)
  end

  test "rejects a malformed authorization header with 401", %{conn: conn} do
    conn =
      conn
      |> put_req_header("content-type", "application/json")
      |> put_req_header("authorization", "Bearer")
      |> post("/internal/events", Jason.encode!(envelope()))

    assert %{"error" => %{"code" => "unauthorized"}} = json_response(conn, 401)
  end

  # -- validation -------------------------------------------------------

  test "rejects an unknown event with 422", %{conn: conn} do
    conn = post_event(conn, envelope(%{"event" => "post:like"}))
    assert %{"error" => %{"code" => "unknown_event"}} = json_response(conn, 422)
  end

  test "rejects a non-user topic with 422", %{conn: conn} do
    conn = post_event(conn, envelope(%{"topics" => ["room:general"]}))
    assert %{"error" => %{"code" => "invalid_topic"}} = json_response(conn, 422)
  end

  test "rejects an empty topics list with 422", %{conn: conn} do
    conn = post_event(conn, envelope(%{"topics" => []}))
    assert %{"error" => %{"code" => "invalid_topics"}} = json_response(conn, 422)
  end

  test "rejects a missing dedupe_key with 422", %{conn: conn} do
    conn = post_event(conn, envelope(%{"dedupe_key" => ""}))
    assert %{"error" => %{"code" => "missing_dedupe_key"}} = json_response(conn, 422)
  end

  test "rejects a payload missing required fields with 422", %{conn: conn} do
    conn = post_event(conn, envelope(%{"payload" => %{"text" => "no ids"}}))
    assert %{"error" => %{"code" => "invalid_payload"}} = json_response(conn, 422)
  end

  # -- happy path -------------------------------------------------------

  test "accepts a valid envelope with 202 and broadcasts it on the user topics", %{
    conn: conn
  } do
    :ok = SutraChatWeb.Endpoint.subscribe("user:alice")
    :ok = SutraChatWeb.Endpoint.subscribe("user:bob")

    conn = post_event(conn, envelope(%{"topics" => ["user:alice", "user:bob"]}))

    assert %{"status" => "accepted", "event" => "post:new", "topics" => 2} =
             json_response(conn, 202)

    for topic <- ["user:alice", "user:bob"] do
      assert_receive %Phoenix.Socket.Broadcast{
        topic: ^topic,
        event: "post:new",
        payload: %{"post_id" => "123", "text" => "hello"}
      }
    end
  end

  test "accepts follower:new and fans it out", %{conn: conn} do
    :ok = SutraChatWeb.Endpoint.subscribe("user:bob")

    conn =
      post_event(
        conn,
        envelope(%{
          "topics" => ["user:bob"],
          "event" => "follower:new",
          "dedupe_key" => "follow:aaa:bbb",
          "payload" => %{
            "follower_id" => "aaa",
            "follower_handle" => "alice",
            "created_at" => "2026-10-18T10:00:00Z"
          }
        })
      )

    assert %{"status" => "accepted", "event" => "follower:new", "topics" => 1} =
             json_response(conn, 202)

    assert_receive %Phoenix.Socket.Broadcast{
      topic: "user:bob",
      event: "follower:new",
      payload: %{"follower_id" => "aaa"}
    }
  end

  test "accepts shipment:update and fans it out to the buyer topic", %{conn: conn} do
    :ok = SutraChatWeb.Endpoint.subscribe("user:buyer-123")

    conn =
      post_event(
        conn,
        envelope(%{
          "topics" => ["user:buyer-123"],
          "event" => "shipment:update",
          "dedupe_key" => "shipment:s1:in_transit:1729219200",
          "payload" => %{
            "shipment_id" => "s1",
            "order_id" => "o1",
            "status" => "in_transit",
            "tracking_number" => "DLV123456789",
            "cod_expected_paise" => 99900,
            "updated_at" => "2026-10-18T12:00:00Z"
          }
        })
      )

    assert %{"status" => "accepted", "event" => "shipment:update", "topics" => 1} =
             json_response(conn, 202)

    assert_receive %Phoenix.Socket.Broadcast{
      topic: "user:buyer-123",
      event: "shipment:update",
      payload: %{"shipment_id" => "s1", "status" => "in_transit"}
    }
  end

  test "rejects shipment:update with a non-integer cod_expected_paise", %{conn: conn} do
    conn =
      post_event(
        conn,
        envelope(%{
          "event" => "shipment:update",
          "dedupe_key" => "shipment:s1:in_transit:1729219200",
          "payload" => %{
            "shipment_id" => "s1",
            "order_id" => "o1",
            "status" => "in_transit",
            "cod_expected_paise" => "99900",
            "updated_at" => "2026-10-18T12:00:00Z"
          }
        })
      )

    assert %{"error" => %{"code" => "invalid_payload"}} = json_response(conn, 422)
  end

  test "rejects shipment:update with a missing required field", %{conn: conn} do
    conn =
      post_event(
        conn,
        envelope(%{
          "event" => "shipment:update",
          "dedupe_key" => "shipment:s1:in_transit:1729219200",
          "payload" => %{
            "shipment_id" => "s1",
            "order_id" => "o1",
            # status missing
            "cod_expected_paise" => 99900,
            "updated_at" => "2026-10-18T12:00:00Z"
          }
        })
      )

    assert %{"error" => %{"code" => "invalid_payload"}} = json_response(conn, 422)
  end
end
