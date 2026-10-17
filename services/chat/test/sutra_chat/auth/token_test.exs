# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Auth.TokenTest do
  use ExUnit.Case, async: true

  alias SutraChat.Auth.Token

  @issuer "https://issuer.example"
  @audience "sutra-api"
  @opts %{issuer: @issuer, audience: @audience}

  setup do
    # Fresh RSA keypair per test run; the JWK map mirrors what
    # TokenVerifier builds from a real JWKS document.
    priv = :public_key.generate_key({:rsa, 2048, 65_537})
    {:RSAPrivateKey, _v, n, e, _d, _p, _q, _dp, _dq, _qinv, _other} = priv
    pub = {:RSAPublicKey, n, e}
    keys = %{"test-key" => pub}
    %{priv: priv, keys: keys, n: n, e: e}
  end

  defp b64url(bin), do: Base.url_encode64(bin, padding: false)

  defp mint(priv, header, payload) do
    h = header |> Jason.encode!() |> b64url()
    p = payload |> Jason.encode!() |> b64url()
    sig = :public_key.sign(h <> "." <> p, :sha256, priv) |> b64url()
    h <> "." <> p <> "." <> sig
  end

  defp payload(overrides \\ %{}) do
    Map.merge(
      %{
        "sub" => "user-123",
        "iss" => @issuer,
        "aud" => @audience,
        "exp" => System.system_time(:second) + 3600,
        "iat" => System.system_time(:second)
      },
      overrides
    )
  end

  defp header(overrides \\ %{}) do
    Map.merge(%{"alg" => "RS256", "typ" => "JWT", "kid" => "test-key"}, overrides)
  end

  test "valid token verifies and returns claims", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload(%{"name" => "Ada"}))

    assert {:ok, claims} = Token.verify(token, keys, @opts)
    assert claims.subject == "user-123"
    assert claims.issuer == @issuer
    assert claims.audience == [@audience]
    assert claims.extra == %{"name" => "Ada"}
  end

  test "aud as a list is accepted when it contains the audience", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload(%{"aud" => ["other", @audience]}))
    assert {:ok, %Token.Claims{audience: ["other", @audience]}} = Token.verify(token, keys, @opts)
  end

  test "tampered signature is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload())
    [h, p, _s] = String.split(token, ".")
    assert {:error, :invalid_signature} = Token.verify(h <> "." <> p <> "." <> b64url("bogus"), keys, @opts)
  end

  test "token signed by a different key is rejected", %{keys: keys} do
    other_priv = :public_key.generate_key({:rsa, 2048, 65_537})
    token = mint(other_priv, header(), payload())
    assert {:error, :invalid_signature} = Token.verify(token, keys, @opts)
  end

  test "alg:none is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(%{"alg" => "none"}), payload())
    assert {:error, :unsupported_algorithm} = Token.verify(token, keys, @opts)
  end

  test "HS256 is rejected (algorithm confusion)", %{priv: priv, keys: keys} do
    token = mint(priv, header(%{"alg" => "HS256"}), payload())
    assert {:error, :unsupported_algorithm} = Token.verify(token, keys, @opts)
  end

  test "missing kid is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(%{"kid" => nil}), payload())
    assert {:error, :missing_kid} = Token.verify(token, keys, @opts)
  end

  test "unknown kid is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(%{"kid" => "rotated-key"}), payload())
    assert {:error, :unknown_kid} = Token.verify(token, keys, @opts)
  end

  test "expired token is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload(%{"exp" => System.system_time(:second) - 10}))
    assert {:error, :token_expired} = Token.verify(token, keys, @opts)
  end

  test "token expiring within the skew leeway is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload(%{"exp" => System.system_time(:second) + 30}))
    assert {:error, :token_expired} = Token.verify(token, keys, @opts)
  end

  test "wrong issuer is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload(%{"iss" => "https://evil.example"}))
    assert {:error, :unexpected_issuer} = Token.verify(token, keys, @opts)
  end

  test "wrong audience is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload(%{"aud" => "other-api"}))
    assert {:error, :audience_not_granted} = Token.verify(token, keys, @opts)
  end

  test "missing subject is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload(%{"sub" => nil}))
    assert {:error, :missing_subject} = Token.verify(token, keys, @opts)
  end

  test "missing expiry is rejected", %{priv: priv, keys: keys} do
    token = mint(priv, header(), payload(%{"exp" => nil}))
    assert {:error, :missing_expiry} = Token.verify(token, keys, @opts)
  end

  test "malformed tokens are rejected", %{keys: keys} do
    assert {:error, :malformed_token} = Token.verify("not-a-token", keys, @opts)
    assert {:error, :malformed_token} = Token.verify("a.b", keys, @opts)
    assert {:error, :malformed_token} = Token.verify("..", keys, @opts)
    assert {:error, :malformed_token} = Token.verify("", keys, @opts)
    assert {:error, :malformed_token} = Token.verify(nil, keys, @opts)
  end

  test "peek_kid returns the kid without verifying", %{priv: priv, keys: _keys} do
    token = mint(priv, header(%{"kid" => "some-key"}), payload())
    assert {:ok, "some-key"} = Token.peek_kid(token)
    assert {:error, :unsupported_algorithm} = Token.peek_kid(mint(priv, header(%{"alg" => "none"}), payload()))
  end

  test "rsa_public_key parses a JWK and rejects junk", %{n: n, e: e} do
    jwk = %{
      "kty" => "RSA",
      "kid" => "k1",
      "n" => b64url(:binary.encode_unsigned(n)),
      "e" => b64url(:binary.encode_unsigned(e))
    }

    assert {:ok, {:RSAPublicKey, ^n, ^e}} = Token.rsa_public_key(jwk)
    assert {:error, :bad_jwk} = Token.rsa_public_key(%{"kty" => "EC", "kid" => "k1"})
    assert {:error, :bad_jwk} = Token.rsa_public_key(%{"kty" => "RSA", "kid" => "", "n" => "x", "e" => "y"})
    assert {:error, :bad_jwk} = Token.rsa_public_key(%{"kty" => "RSA", "kid" => "k1", "n" => "!!!", "e" => "AQAB"})
  end
end
