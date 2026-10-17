# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Auth.TokenVerifier do
  @moduledoc """
  GenServer holding the OIDC JWKS cache used for websocket authentication.

  Mirrors the Go API service's `internal/oidc.Verifier` lifecycle:

    * keys are fetched lazily on the first verification, so the chat
      service can boot before the identity issuer (Hydra) is up;
    * the cache is refreshed when it goes stale (15 minutes) or when a
      token arrives with an unknown `kid` (key rotation);
    * because all verifications serialize through this one process, a
      refresh is never thundering-herded — concurrent connects wait on a
      single in-flight fetch.

  Configuration comes from the `:sutra_chat, :auth` app env, populated
  from the SAME environment variables the Go API uses
  (`HYDRA_ISSUER`, `JWT_AUDIENCE`, `AUTH_BYPASS`, `AUTH_BYPASS_SUBJECT`),
  so operators configure identity once for both services. The JWKS
  document is expected at `<issuer>/.well-known/jwks.json`, exactly like
  the Go side.
  """
  use GenServer
  require Logger

  alias SutraChat.Auth.Token

  # How long fetched keys are trusted before a background refresh,
  # mirroring the Go API's 15-minute JWKS TTL.
  @jwks_ttl_ms 15 * 60 * 1_000
  @http_timeout_ms 10_000
  @call_timeout_ms 20_000

  def start_link(opts \\ []) do
    GenServer.start_link(__MODULE__, opts, name: __MODULE__)
  end

  @doc """
  Verifies a bearer token against the issuer's JWKS.

  Returns `{:ok, %Token.Claims{}}` with the token's subject, or
  `{:error, reason}` (see `SutraChat.Auth.Token.verify/3` for reasons,
  plus `:jwks_fetch_failed` / `{:jwks_http_status, status}` /
  `:bad_jwks_document` when the issuer is unreachable or misbehaving).

  With `AUTH_BYPASS=1` this short-circuits and returns the configured
  bypass subject without touching the network — local development only,
  loudly logged at startup.
  """
  @spec verify(String.t()) :: {:ok, Token.Claims.t()} | {:error, term()}
  def verify(token) do
    if bypass?() do
      {:ok,
       %Token.Claims{
         subject: bypass_subject(),
         expires_at: System.system_time(:second) + 3_600
       }}
    else
      GenServer.call(__MODULE__, {:verify, token}, @call_timeout_ms)
    end
  end

  @impl true
  def init(opts) do
    cfg = Keyword.merge(auth_config(), opts)
    # Same defaults as the Go API's config package.
    issuer = Keyword.get(cfg, :issuer, "http://hydra:4444")
    audience = Keyword.get(cfg, :audience, "sutra-api")
    jwks_url = jwks_url(issuer, Keyword.get(cfg, :jwks_url))

    if bypass?() do
      Logger.warning(
        "AUTH_BYPASS=1: websocket JWT verification DISABLED — " <>
          "every socket connects as #{inspect(bypass_subject())}"
      )
    end

    {:ok,
     %{
       issuer: issuer,
       audience: audience,
       jwks_url: jwks_url,
       keys: %{},
       fetched_at: nil
     }}
  end

  @impl true
  def handle_call({:verify, token}, _from, state) do
    with {:ok, kid} <- Token.peek_kid(token),
         {:ok, state} <- ensure_key(kid, state) do
      result = Token.verify(token, state.keys, %{issuer: state.issuer, audience: state.audience})
      {:reply, result, state}
    else
      {:error, reason} -> {:reply, {:error, reason}, state}
    end
  end

  # -- JWKS cache -----------------------------------------------------

  # Returns the key for kid, refreshing the cache when the key is missing
  # (rotation) or the cache is stale. A failed refresh keeps the old cache
  # untouched and reports the failure instead of half-updating state.
  defp ensure_key(kid, %{keys: keys, fetched_at: fetched_at} = state) do
    if Map.has_key?(keys, kid) and not stale?(fetched_at) do
      {:ok, state}
    else
      case refresh(state) do
        {:ok, refreshed} ->
          if Map.has_key?(refreshed.keys, kid) do
            {:ok, refreshed}
          else
            {:error, :unknown_kid}
          end

        {:error, reason} ->
          {:error, reason}
      end
    end
  end

  defp stale?(nil), do: true

  defp stale?(fetched_at) do
    System.monotonic_time(:millisecond) - fetched_at > @jwks_ttl_ms
  end

  defp refresh(state) do
    case fetch_jwks(state.jwks_url) do
      {:ok, keys} ->
        {:ok, %{state | keys: keys, fetched_at: System.monotonic_time(:millisecond)}}

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp fetch_jwks(url) do
    http_opts = [timeout: @http_timeout_ms, connect_timeout: 5_000]

    case :httpc.request(:get, {String.to_charlist(url), []}, http_opts, body_format: :binary) do
      {:ok, {{_, 200, _}, _headers, body}} ->
        parse_jwks(body)

      {:ok, {{_, status, _}, _headers, _body}} ->
        {:error, {:jwks_http_status, status}}

      {:error, reason} ->
        {:error, {:jwks_fetch_failed, reason}}
    end
  end

  defp parse_jwks(body) do
    with {:ok, %{"keys" => jwks}} <- Jason.decode(body),
         true <- is_list(jwks) do
      keys =
        for jwk <- jwks,
            {:ok, pub} <- [Token.rsa_public_key(jwk)],
            kid = jwk["kid"],
            into: %{} do
          {kid, pub}
        end

      {:ok, keys}
    else
      _ -> {:error, :bad_jwks_document}
    end
  end

  # -- config ---------------------------------------------------------

  defp auth_config, do: Application.get_env(:sutra_chat, :auth, [])

  defp bypass?, do: Keyword.get(auth_config(), :bypass, false) == true

  defp bypass_subject do
    Keyword.get(auth_config(), :bypass_subject, "dev-user") || "dev-user"
  end

  defp jwks_url(_issuer, override) when is_binary(override) and override != "" do
    override
  end

  defp jwks_url(issuer, _override) do
    String.trim_trailing(issuer, "/") <> "/.well-known/jwks.json"
  end
end
