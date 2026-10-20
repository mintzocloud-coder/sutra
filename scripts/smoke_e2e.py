#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
"""I1 E2E device-key round-trip for scripts/smoke.sh.

Proves the full I1 path against the real stack:

    openssl-minted bundle → POST /v1/e2e/devices (Go register)
    → bad-signature register 422s (anti-MITM check)
    → GET /v1/users/{id}/devices (directory)
    → REST GET prekey-bundle consumes id 1
    → websocket `user:<subject>` join → `e2e:prekey_fetch` consumes id 2
      (chat-side atomic consume of the SAME queue)
    → REST GET prekey-bundle consumes id 3
    → websocket fetch on the drained queue → one_time_prekey null
      (signed-prekey-only fallback, not an error)
    → websocket fetch unknown device → not_found
    → websocket fetch empty payload → invalid_request
    → DELETE the device (cleanup) → directory empty, consume 404s

The consume ordering (REST 1, WS 2, REST 3, dry on WS) proves the Go
API and the chat service share one atomic one-time-prekey queue —
no duplicate hand-out across the two paths.

Usage:
    python3 scripts/smoke_e2e.py <api-url> <chat-host> <chat-port> \
        <api-token> <target-subject> <ws-subject>
  <api-token>      Hydra-issued JWT for the Go API (bypass off).
  <target-subject> OIDC subject registering the device (the Hydra
                   client id when minted via client_credentials).
  <ws-subject>     subject the chat websocket connects as (bypass
                   subject in smoke, since the chat boots with
                   AUTH_BYPASS=1); joins user:<ws-subject>.
Exits 0 on success, raises on any mismatch.
"""

import base64
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

from smoke_ws import PhoenixClient


class E2EError(Exception):
    pass


def _openssl(*args, input_data=None, cwd=None):
    try:
        return subprocess.run(
            ["openssl", *args], input=input_data, capture_output=True,
            check=True, cwd=cwd,
        ).stdout
    except (subprocess.CalledProcessError, FileNotFoundError) as e:
        raise E2EError(f"openssl {' '.join(args)} failed: {e}")


def _raw_pubkey(priv_pem):
    """Last 32 bytes of the DER SubjectPublicKeyInfo = the raw key."""
    der = _openssl("pkey", "-in", priv_pem, "-pubout", "-outform", "DER")
    return der[-32:]


def mint_bundle(workdir, n_prekeys=3):
    """openssl-mint a full device key bundle. Returns dict of raw bytes
    plus the identity private key path (for the bad-signature case)."""
    id_priv = os.path.join(workdir, "id.pem")
    sp_priv = os.path.join(workdir, "sp.pem")
    _openssl("genpkey", "-algorithm", "ed25519", "-out", id_priv)
    _openssl("genpkey", "-algorithm", "X25519", "-out", sp_priv)
    os.chmod(id_priv, 0o600)
    id_pub = _raw_pubkey(id_priv)
    sp_pub = _raw_pubkey(sp_priv)
    sp_raw = os.path.join(workdir, "sp.raw")
    with open(sp_raw, "wb") as f:
        f.write(sp_pub)
    sig = _openssl("pkeyutl", "-sign", "-inkey", id_priv, "-rawin", "-in", sp_raw)
    prekeys = []
    for i in range(1, n_prekeys + 1):
        p = os.path.join(workdir, f"otp{i}.pem")
        _openssl("genpkey", "-algorithm", "X25519", "-out", p)
        prekeys.append((i, _raw_pubkey(p)))
    return {
        "id_priv": id_priv, "id_pub": id_pub, "sp_pub": sp_pub,
        "sp_sig": sig, "prekeys": prekeys,
    }


def mint_bad_sig_bundle(workdir):
    """Same as mint_bundle but the signature is made by a DIFFERENT
    Ed25519 key — the server must reject it (anti-MITM)."""
    b = mint_bundle(workdir)
    other_priv = os.path.join(workdir, "other.pem")
    _openssl("genpkey", "-algorithm", "ed25519", "-out", other_priv)
    sp_raw = os.path.join(workdir, "sp.raw")
    bad_sig = _openssl("pkeyutl", "-sign", "-inkey", other_priv, "-rawin", "-in", sp_raw)
    b["sp_sig"] = bad_sig
    return b


def b64(b):
    return base64.b64encode(b).decode("ascii")


def register_body(bundle, device_id, registration_id=4242):
    return {
        "device_id": device_id,
        "registration_id": registration_id,
        "identity_key": b64(bundle["id_pub"]),
        "signed_prekey": b64(bundle["sp_pub"]),
        "signed_prekey_id": 7,
        "signed_prekey_sig": b64(bundle["sp_sig"]),
        "one_time_prekeys": [
            {"id": pid, "public_key": b64(pub)} for pid, pub in bundle["prekeys"]
        ],
    }


def api(method, url, token, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        url, data=data, method=method,
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
            return r.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, (json.loads(raw) if raw else None)


def check(cond, msg):
    if not cond:
        raise E2EError(msg)


def main():
    api_url, chat_host, chat_port = sys.argv[1], sys.argv[2], int(sys.argv[3])
    token, target_subject, ws_subject = sys.argv[4], sys.argv[5], sys.argv[6]
    device_id = "smoke-e2e-device"

    workdir = tempfile.mkdtemp(prefix="sutra-e2e-")
    try:
        # 1. Register the openssl-minted bundle via the Go API.
        bundle = mint_bundle(workdir)
        status, body = api("POST", f"{api_url}/v1/e2e/devices", token,
                           register_body(bundle, device_id))
        check(status == 201, f"register: expected 201, got {status}: {body}")
        check(body["device_id"] == device_id, f"register: device_id mismatch: {body}")
        check(body["available_prekeys"] == 3, f"register: expected 3 prekeys: {body}")
        print("smoke PASS: e2e register 201 (openssl-minted bundle, 3 one-time prekeys)")

        # 2. Bad-signature bundle must be rejected — the anti-MITM check.
        bad = mint_bad_sig_bundle(workdir)
        status, body = api("POST", f"{api_url}/v1/e2e/devices", token,
                           register_body(bad, "smoke-e2e-bad"))
        check(status == 422, f"bad-sig register: expected 422, got {status}: {body}")
        check(body["error"]["code"] == "e2e_invalid_bundle",
              f"bad-sig register: wrong envelope code: {body}")
        print("smoke PASS: e2e register with cross-key signature -> 422 e2e_invalid_bundle")

        # 3. Directory lists the device (public material only).
        me_status, me = api("GET", f"{api_url}/v1/me", token)
        check(me_status == 200, f"/v1/me: expected 200, got {me_status}: {me}")
        user_id = me["id"]
        check(me["subject"] == target_subject, f"/v1/me subject {me['subject']} != {target_subject}")
        status, directory = api("GET", f"{api_url}/v1/users/{user_id}/devices", token)
        check(status == 200, f"directory: expected 200, got {status}: {directory}")
        devs = directory if isinstance(directory, list) else directory.get("devices", [])
        check(any(d["device_id"] == device_id for d in devs),
              f"directory missing the device: {directory}")
        print("smoke PASS: e2e directory lists the registered device")

        # 4. REST consume hands out one-time prekey id 1.
        status, b1 = api("GET",
                         f"{api_url}/v1/users/{user_id}/devices/{device_id}/prekey-bundle",
                         token)
        check(status == 200, f"prekey-bundle: expected 200, got {status}: {b1}")
        check(b1["identity_key"] == b64(bundle["id_pub"]), "bundle identity_key mismatch")
        check(b1["signed_prekey"] == b64(bundle["sp_pub"]), "bundle signed_prekey mismatch")
        check(b1["one_time_prekey"]["id"] == 1, f"REST consume: expected id 1: {b1}")
        check(b1["available_prekeys"] == 2, f"REST consume: expected 2 left: {b1}")
        print("smoke PASS: e2e REST prekey-bundle consumed id 1 (2 left)")

        # 5. Websocket leg: join own user:<subject> topic, fetch the SAME
        #    user's bundle — the chat must hand out id 2 (never id 1 again).
        client = PhoenixClient(chat_host, chat_port, token="smoke-test-token")
        join_resp = client.join(f"user:{ws_subject}")
        check(join_resp is not None, f"join user:{ws_subject} failed")
        reply = client.push_and_wait_reply(
            f"user:{ws_subject}", "e2e:prekey_fetch",
            {"user_id": target_subject, "device_id": device_id})
        check(reply["status"] == "ok", f"e2e:prekey_fetch rejected: {reply}")
        ws_bundle = reply["response"]
        check(ws_bundle["identity_key"] == b64(bundle["id_pub"]),
              "ws bundle identity_key mismatch")
        check(ws_bundle["one_time_prekey"]["id"] == 2,
              f"WS consume: expected id 2, got {ws_bundle}")
        check(ws_bundle["available_prekeys"] == 1,
              f"WS consume: expected 1 left, got {ws_bundle}")
        print("smoke PASS: e2e websocket e2e:prekey_fetch consumed id 2 (1 left)")

        # 6. REST consume hands out id 3 — the last one.
        status, b3 = api("GET",
                         f"{api_url}/v1/users/{user_id}/devices/{device_id}/prekey-bundle",
                         token)
        check(status == 200, f"prekey-bundle 2: expected 200, got {status}: {b3}")
        check(b3["one_time_prekey"]["id"] == 3, f"REST consume: expected id 3: {b3}")
        check(b3["available_prekeys"] == 0, f"REST consume: expected 0 left: {b3}")
        print("smoke PASS: e2e REST prekey-bundle consumed id 3 (queue now empty)")

        # 7. Drained queue over the websocket: bundle still returns with
        #    one_time_prekey null (signed-prekey-only X3DH fallback).
        reply = client.push_and_wait_reply(
            f"user:{ws_subject}", "e2e:prekey_fetch",
            {"user_id": target_subject, "device_id": device_id})
        check(reply["status"] == "ok", f"drained fetch rejected: {reply}")
        dry = reply["response"]
        check(dry["one_time_prekey"] is None, f"drained fetch: expected null prekey: {dry}")
        check(dry["available_prekeys"] == 0, f"drained fetch: expected 0 left: {dry}")
        print("smoke PASS: e2e websocket fetch on drained queue -> one_time_prekey null")

        # 8. Negative cases over the websocket.
        reply = client.push_and_wait_reply(
            f"user:{ws_subject}", "e2e:prekey_fetch",
            {"user_id": target_subject, "device_id": "ghost-device"})
        check(reply["status"] == "error" and reply["response"].get("reason") == "not_found",
              f"unknown device: expected not_found, got {reply}")
        reply = client.push_and_wait_reply(
            f"user:{ws_subject}", "e2e:prekey_fetch", {})
        check(reply["status"] == "error" and reply["response"].get("reason") == "invalid_request",
              f"empty payload: expected invalid_request, got {reply}")
        print("smoke PASS: e2e websocket fetch -> not_found / invalid_request")
        client.close()

        # 9. Cleanup: delete the device; directory empty; consume 404s.
        status, _ = api("DELETE", f"{api_url}/v1/e2e/devices/{device_id}", token)
        check(status == 204, f"delete device: expected 204, got {status}")
        status, directory = api("GET", f"{api_url}/v1/users/{user_id}/devices", token)
        devs = directory if isinstance(directory, list) else directory.get("devices", [])
        check(all(d["device_id"] != device_id for d in devs),
              f"device still listed after delete: {directory}")
        status, body = api("GET",
                           f"{api_url}/v1/users/{user_id}/devices/{device_id}/prekey-bundle",
                           token)
        check(status == 404, f"consume after delete: expected 404, got {status}: {body}")
        check(body["error"]["code"] == "e2e_device_not_found",
              f"consume after delete: wrong envelope code: {body}")
        print("smoke PASS: e2e device deleted; directory empty; consume -> 404")
    finally:
        shutil.rmtree(workdir, ignore_errors=True)

    print("smoke PASS: I1 E2E device-key round-trip "
          "(register → 422 bad-sig → directory → REST/WS/REST consume 1-2-3 "
          "→ dry-queue fallback → negatives → delete)")


if __name__ == "__main__":
    main()
