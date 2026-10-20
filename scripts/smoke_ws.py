#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Chat websocket round-trip check for scripts/smoke.sh (C1).

Pure-stdlib Phoenix channels v2 client. Connects to the chat service,
joins a fresh room, sends one message, and asserts the full server path:

    join → phx_join ok + empty history
    message:new push → broadcast message:new + msg:ack (sender only)
    reconnect + rejoin → the message is in history (survived = persisted)

Usage: python3 scripts/smoke_ws.py <host> <port>
Exits 0 on success, raises on any mismatch.
"""

import base64
import hashlib
import json
import secrets
import socket
import struct
import sys
import time
import urllib.parse


class WsError(Exception):
    pass


def _recvn(sock, n, deadline):
    buf = b""
    while len(buf) < n:
        remaining = deadline - time.time()
        if remaining <= 0:
            raise WsError("timed out reading from socket")
        sock.settimeout(remaining)
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise WsError("connection closed by peer")
        buf += chunk
    return buf


def ws_connect(host, port, path):
    """Raw websocket handshake; returns the connected socket."""
    sock = socket.create_connection((host, port), timeout=10)
    key = base64.b64encode(secrets.token_bytes(16)).decode()
    req = (
        f"GET {path} HTTP/1.1\r\n"
        f"Host: {host}:{port}\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        "\r\n"
    )
    sock.sendall(req.encode())
    resp = b""
    deadline = time.time() + 10
    while b"\r\n\r\n" not in resp:
        resp += _recvn(sock, 1, deadline)
    status_line = resp.split(b"\r\n", 1)[0]
    if b"101" not in status_line:
        raise WsError(f"websocket handshake failed: {status_line!r}")
    accept = None
    for line in resp.split(b"\r\n"):
        if line.lower().startswith(b"sec-websocket-accept:"):
            accept = line.split(b":", 1)[1].strip().decode()
    expected = base64.b64encode(
        hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()
    ).decode()
    if accept != expected:
        raise WsError("websocket handshake: bad Sec-WebSocket-Accept")
    return sock


def ws_send_frame(sock, opcode, payload):
    """Send one masked client frame."""
    mask = secrets.token_bytes(4)
    n = len(payload)
    header = bytearray([0x80 | opcode])
    if n < 126:
        header.append(0x80 | n)
    elif n < 65536:
        header.append(0x80 | 126)
        header += struct.pack("!H", n)
    else:
        header.append(0x80 | 127)
        header += struct.pack("!Q", n)
    header += mask
    sock.sendall(bytes(header) + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))


def ws_recv_text(sock, deadline):
    """Read frames until a text frame arrives; answer pings, skip others."""
    while True:
        hdr = _recvn(sock, 2, deadline)
        b1, b2 = hdr[0], hdr[1]
        opcode = b1 & 0x0F
        masked = b2 & 0x80
        length = b2 & 0x7F
        if length == 126:
            length = struct.unpack("!H", _recvn(sock, 2, deadline))[0]
        elif length == 127:
            length = struct.unpack("!Q", _recvn(sock, 8, deadline))[0]
        mask = _recvn(sock, 4, deadline) if masked else None
        payload = _recvn(sock, length, deadline)
        if mask:
            payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        if opcode == 0x8:
            raise WsError("server sent close frame")
        if opcode == 0x9:  # ping -> pong, keep reading
            ws_send_frame(sock, 0xA, payload)
            continue
        if opcode == 0x1:
            return payload.decode("utf-8")
        # binary / continuation frames are not part of this protocol; skip


class PhoenixClient:
    """Minimal Phoenix channels v2 (JSON serializer) client."""

    def __init__(self, host, port, token):
        query = urllib.parse.urlencode({"token": token, "vsn": "2.0.0"})
        self.sock = ws_connect(host, port, f"/socket/websocket?{query}")
        self._ref = 0
        self._join_refs = {}

    def _next_ref(self):
        self._ref += 1
        return str(self._ref)

    def _wait(self, predicate, timeout, description):
        deadline = time.time() + timeout
        while time.time() < deadline:
            raw = ws_recv_text(self.sock, deadline)
            try:
                msg = json.loads(raw)
            except json.JSONDecodeError:
                continue
            if not isinstance(msg, list) or len(msg) != 5:
                continue
            if predicate(msg):
                return msg
        raise WsError(f"timed out waiting for {description}")

    def join(self, topic, params=None, timeout=15):
        ref = self._next_ref()
        self._join_refs[topic] = ref
        ws_send_frame(self.sock, 0x1, json.dumps([ref, ref, topic, "phx_join", params or {}]).encode())

        def is_reply(msg):
            return msg[2] == topic and msg[3] == "phx_reply" and msg[1] == ref

        reply = self._wait(is_reply, timeout, f"phx_reply to join {topic}")
        if reply[4].get("status") != "ok":
            raise WsError(f"join {topic} rejected: {reply[4]}")
        return reply[4]["response"]

    def push(self, topic, event, payload):
        ws_send_frame(
            self.sock,
            0x1,
            json.dumps([self._join_refs[topic], self._next_ref(), topic, event, payload]).encode(),
        )

    def push_and_wait_reply(self, topic, event, payload, timeout=15):
        """Push an event and wait for its phx_reply; returns the reply
        payload ({"status": ..., "response": ...})."""
        ref = self._next_ref()
        ws_send_frame(
            self.sock,
            0x1,
            json.dumps([self._join_refs[topic], ref, topic, event, payload]).encode(),
        )

        def is_reply(msg):
            return msg[2] == topic and msg[3] == "phx_reply" and msg[1] == ref

        reply = self._wait(is_reply, timeout, f"phx_reply to {event} on {topic}")
        return reply[4]

    def wait_event(self, topic, event, timeout=15):
        def is_event(msg):
            return msg[2] == topic and msg[3] == event

        return self._wait(is_event, timeout, f"event {event} on {topic}")[4]

    def close(self):
        try:
            ws_send_frame(self.sock, 0x8, b"")
        finally:
            self.sock.close()


def main():
    host, port = sys.argv[1], int(sys.argv[2])
    room = f"smoke-{secrets.token_hex(4)}"
    topic = f"room:{room}"
    body = "hello from the sutra smoke test"
    client_msg_id = secrets.token_hex(16)

    # 1. Connect + join a fresh room: join must be ok, history empty.
    client = PhoenixClient(host, port, token="smoke-test-token")
    join_resp = client.join(topic, {"device_id": "smoke-script"})
    assert join_resp["room_id"] == room, f"join room_id mismatch: {join_resp}"
    assert join_resp["history"]["messages"] == [], "fresh room must join with empty history"

    # 2. Send one message: expect the broadcast AND the sender-only ack.
    client.push(topic, "message:new", {"body": body, "client_msg_id": client_msg_id})
    broadcast = client.wait_event(topic, "message:new")
    assert broadcast["body"] == body, f"broadcast body mismatch: {broadcast}"
    assert broadcast["client_msg_id"] == client_msg_id, "broadcast client_msg_id mismatch"
    assert broadcast["room_id"] == room, "broadcast room_id mismatch"

    ack = client.wait_event(topic, "msg:ack")
    assert ack["client_msg_id"] == client_msg_id, "ack client_msg_id mismatch"
    assert ack["deduped"] is False, f"first send must not be deduped: {ack}"
    assert ack["id"] == broadcast["id"], "ack id must match the broadcast message id"
    client.close()

    # 3. Reconnect and rejoin: the message must be in history (it was
    #    persisted before broadcast, so a restart/reconnect cannot lose it).
    client2 = PhoenixClient(host, port, token="smoke-test-token")
    rejoin_resp = client2.join(topic, {"device_id": "smoke-script-2"})
    history = rejoin_resp["history"]["messages"]
    assert any(
        m["client_msg_id"] == client_msg_id and m["body"] == body for m in history
    ), f"sent message missing from history after reconnect: {history}"
    client2.close()

    print("smoke PASS: chat websocket round-trip "
          "(join → send → broadcast + msg:ack → reconnect → history)")


if __name__ == "__main__":
    main()
