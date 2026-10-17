# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Auth.Token do
  @moduledoc """
  Pure RS256 JWT verification for websocket authentication.

  This is the Elixir half of Sutra's shared identity contract — it mirrors
  the Go API service's `internal/oidc` package claim-for-claim, so a token
  accepted by the REST API is accepted by the chat socket and vice versa:

    * algorithm pinned to RS256. Rejecting `alg: none` and friends is the
      whole point of the pin — it kills algorithm-confusion attacks.
    * `kid` required; the signature is checked against that key only.
    * `exp` enforced with a 60s clock-skew leeway (a token expiring within
      60s is treated as expired, exactly like the Go verifier).
    * `iss` must equal the configured issuer. An empty expected issuer
      disables the check — tests only, never production.
    * `aud` must contain the configured audience, given either as a string
      or a list of strings. An empty expected audience disables the check —
      tests only, never production.
    * `sub` required — it becomes the socket's user identity.

  Verification is stdlib-only (`:public_key`, plus Jason for JSON): no
  third-party JWT library, same as the Go side.

  There is deliberately no I/O here. Key lookup and JWKS fetching live in
  `SutraChat.Auth.TokenVerifier`; `verify/3` takes an explicit key map so
  every path is unit-testable without a network.
  """

  defmodule Claims do
    @moduledoc "Validated claims carried by a verified access token."
    @enforce_keys [:subject, :expires_at]
    defstruct [:subject, :issuer, :audience, :expires_at, :issued_at, :extra]

    @type t :: %__MODULE__{
            subject: String.t(),
            issuer: String.t() | nil,
            audience: [String.t()],
            expires_at: integer(),
            issued_at: integer() | nil,
            extra: map()
          }
  end

  # Tolerates clock skew between the chat service and the issuer.
  # Mirrors the Go API's 60s leeway.
  @expiry_leeway_seconds 60

  @type rsa_public_key :: {:RSAPublicKey, pos_integer(), pos_integer()}
  @type key_map :: %{String.t() => rsa_public_key()}
  @type verify_opts :: %{issuer: String.t(), audience: String.t()}

  @doc """
  Verifies `token` against `keys` (`%{kid => {:RSAPublicKey, n, e}}`).

  Returns `{:ok, %Claims{}}`, or `{:error, reason}` where reason is one of:

    * `:malformed_token` — not three non-empty dot-separated parts
    * `:bad_header` — header is not base64url JSON
    * `:unsupported_algorithm` — `alg` is anything but `"RS256"`
    * `:missing_kid` — no `kid` in the header
    * `:unknown_kid` — no key for `kid` in `keys`
    * `:bad_signature` — signature part is not valid base64url
    * `:invalid_signature` — signature does not verify against the key
    * `:bad_payload` — payload is not base64url JSON
    * `:missing_subject` / `:missing_expiry`
    * `:token_expired` — `exp` is within the skew leeway (or past)
    * `:unexpected_issuer` / `:audience_not_granted`
  """
  @spec verify(String.t(), key_map(), verify_opts()) ::
          {:ok, Claims.t()} | {:error, atom()}
  def verify(token, keys, opts) when is_binary(token) and is_map(keys) and is_map(opts) do
    with {:ok, {header_b64, payload_b64, sig_b64}} <- split(token),
         {:ok, header} <- decode_json(header_b64, :bad_header),
         :ok <- check_alg(header),
         {:ok, kid} <- check_kid(header),
         {:ok, pub} <- lookup_key(keys, kid),
         {:ok, sig} <- decode_b64(sig_b64, :bad_signature),
         :ok <- check_signature(header_b64 <> "." <> payload_b64, sig, pub),
         {:ok, payload} <- decode_json(payload_b64, :bad_payload),
         {:ok, claims} <- build_claims(payload),
         :ok <- check_expiry(claims),
         :ok <- check_issuer(claims, opts),
         :ok <- check_audience(claims, opts) do
      {:ok, claims}
    end
  end

  def verify(_token, _keys, _opts), do: {:error, :malformed_token}

  @doc """
  Peeks at the token header only: returns the `kid` after the same
  structure/alg checks `verify/3` applies. Lets the JWKS cache fetch (or
  refresh for rotation) the right key before a full verification.
  """
  @spec peek_kid(String.t()) :: {:ok, String.t()} | {:error, atom()}
  def peek_kid(token) when is_binary(token) do
    with {:ok, {header_b64, _payload_b64, _sig_b64}} <- split(token),
         {:ok, header} <- decode_json(header_b64, :bad_header),
         :ok <- check_alg(header),
         {:ok, kid} <- check_kid(header) do
      {:ok, kid}
    end
  end

  def peek_kid(_token), do: {:error, :malformed_token}

  @doc """
  Converts an RSA JWK (`%{"kty" => "RSA", "kid" => ..., "n" => ..., "e" => ...}`,
  base64url modulus + exponent per RFC 7517) into an `:RSAPublicKey` tuple
  usable by `:crypto.verify/5`. Non-RSA keys and malformed values are
  rejected — mirroring the Go verifier's key parsing.
  """
  @spec rsa_public_key(map()) :: {:ok, rsa_public_key()} | {:error, :bad_jwk}
  def rsa_public_key(%{
        "kty" => "RSA",
        "kid" => kid,
        "n" => n_b64,
        "e" => e_b64
      })
      when is_binary(kid) and kid != "" and is_binary(n_b64) and is_binary(e_b64) do
    with {:ok, n_bytes} <- decode_b64(n_b64, :bad_jwk),
         {:ok, e_bytes} <- decode_b64(e_b64, :bad_jwk),
         true <- byte_size(e_bytes) in 1..8 do
      n = :binary.decode_unsigned(n_bytes)
      e = :binary.decode_unsigned(e_bytes)

      if n > 0 and e > 0 do
        {:ok, {:RSAPublicKey, n, e}}
      else
        {:error, :bad_jwk}
      end
    else
      _ -> {:error, :bad_jwk}
    end
  end

  def rsa_public_key(_jwk), do: {:error, :bad_jwk}

  # -- token structure ------------------------------------------------

  defp split(token) do
    case String.split(token, ".") do
      [h, p, s] when h != "" and p != "" and s != "" -> {:ok, {h, p, s}}
      _ -> {:error, :malformed_token}
    end
  end

  defp decode_b64(b64, err) do
    case Base.url_decode64(b64, padding: false) do
      {:ok, bytes} -> {:ok, bytes}
      :error -> {:error, err}
    end
  end

  defp decode_json(b64, err) do
    with {:ok, bytes} <- decode_b64(b64, err),
         {:ok, decoded} <- Jason.decode(bytes),
         true <- is_map(decoded) do
      {:ok, decoded}
    else
      _ -> {:error, err}
    end
  end

  # -- header checks --------------------------------------------------

  # RS256 only. This pin is what defeats `alg: none` and
  # RSA->HMAC confusion attacks.
  defp check_alg(%{"alg" => "RS256"}), do: :ok
  defp check_alg(_header), do: {:error, :unsupported_algorithm}

  defp check_kid(%{"kid" => kid}) when is_binary(kid) and kid != "", do: {:ok, kid}
  defp check_kid(_header), do: {:error, :missing_kid}

  defp lookup_key(keys, kid) do
    case Map.fetch(keys, kid) do
      {:ok, pub} -> {:ok, pub}
      :error -> {:error, :unknown_kid}
    end
  end

  defp check_signature(signing_input, sig, pub) do
    # RS256 = RSASSA-PKCS1-v1_5 over SHA-256 of the ASCII signing input,
    # exactly what the Go side verifies with rsa.VerifyPKCS1v15.
    # (:public_key, not :crypto.verify/5 — the latter rejects RSA key
    # tuples on some OTP releases; :public_key.verify/4 is the stable API
    # every Elixir JWT library uses.)
    try do
      if :public_key.verify(signing_input, :sha256, sig, pub) do
        :ok
      else
        {:error, :invalid_signature}
      end
    rescue
      _ -> {:error, :invalid_signature}
    end
  end

  # -- claim checks ---------------------------------------------------

  defp build_claims(payload) when is_map(payload) do
    with {:ok, sub} <- required_string(payload, "sub", :missing_subject),
         {:ok, exp} <- required_number(payload, "exp", :missing_expiry) do
      {:ok,
       %Claims{
         subject: sub,
         issuer: string_or_nil(payload, "iss"),
         audience: normalize_audience(Map.get(payload, "aud")),
         expires_at: trunc(exp),
         issued_at: trunc_or_nil(Map.get(payload, "iat")),
         extra: Map.drop(payload, ["sub", "iss", "aud", "exp", "iat"])
       }}
    end
  end

  defp build_claims(_payload), do: {:error, :bad_payload}

  defp required_string(payload, key, err) do
    case Map.get(payload, key) do
      v when is_binary(v) and v != "" -> {:ok, v}
      _ -> {:error, err}
    end
  end

  defp required_number(payload, key, err) do
    case Map.get(payload, key) do
      v when is_number(v) -> {:ok, v}
      _ -> {:error, err}
    end
  end

  defp string_or_nil(payload, key) do
    case Map.get(payload, key) do
      v when is_binary(v) -> v
      _ -> nil
    end
  end

  defp trunc_or_nil(v) when is_number(v), do: trunc(v)
  defp trunc_or_nil(_), do: nil

  # The aud claim is legally a string or a list of strings (RFC 7519).
  defp normalize_audience(aud) when is_binary(aud), do: [aud]
  defp normalize_audience(aud) when is_list(aud), do: Enum.filter(aud, &is_binary/1)
  defp normalize_audience(_aud), do: []

  defp check_expiry(%Claims{expires_at: exp}) do
    # Same rule as the Go verifier: now + leeway past exp means expired.
    if exp < System.system_time(:second) + @expiry_leeway_seconds do
      {:error, :token_expired}
    else
      :ok
    end
  end

  defp check_issuer(_claims, %{issuer: ""}), do: :ok

  defp check_issuer(%Claims{issuer: iss}, %{issuer: expected}) do
    if iss == expected, do: :ok, else: {:error, :unexpected_issuer}
  end

  defp check_audience(_claims, %{audience: ""}), do: :ok

  defp check_audience(%Claims{audience: aud}, %{audience: expected}) do
    if expected in aud, do: :ok, else: {:error, :audience_not_granted}
  end
end
