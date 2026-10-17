#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Chat websocket load benchmark for the Sutra chat service (C4).

Pure-stdlib Phoenix channels v2 client. Spawns N concurrent connections,
each joining its own room and sending M messages, and measures:

  - connection rate (successful joins/sec)
  - message throughput (server-confirmed msg:ack per second)
  - ack latency (push -> msg:ack round-trip) p50/p95/p99

The number is END-TO-END on one node: every message is persisted to
PostgreSQL before broadcast (persist-before-broadcast), so throughput
includes the PG write. It also includes presence join/leave diffs and the
Python client's own per-thread overhead — this measures the service,
not a theoretical protocol ceiling.

Usage:
    # 1. boot the stack and keep it up (see scripts/smoke.sh / docs/dev-setup.md)
    SMOKE_KEEP_UP=1 AUTH_BYPASS=1 bash scripts/smoke.sh
    # 2. hammer the chat endpoint
    python3 scripts/bench_ws.py localhost 4000 --conns 200 --msgs 50

Identity: the chat socket takes a `token` param; with AUTH_BYPASS=1 the
smoke stack accepts any token (same dev-only hatch as smoke_ws.py).

Prints a JSON summary on stdout and exits 0. Non-zero exit = the harness
itself failed (bad args, unreachable server, assertion broken); use the
"acks_ok"/"errors" fields to judge the SERVICE, not the exit code —
unless --fail-on-errors is given, in which case dropped messages or
failed connections also exit non-zero (used by scripts/smoke.sh).
"""

import argparse
import json
import secrets
import sys
import threading
import time

from smoke_ws import PhoenixClient, WsError, ws_recv_text


def pct(sorted_vals, p):
    """p-th percentile of an already-sorted list (nearest-rank)."""
    if not sorted_vals:
        return None
    idx = min(len(sorted_vals) - 1, int((p / 100.0) * len(sorted_vals)))
    return sorted_vals[idx]


def summarize(vals):
    vals = sorted(vals)
    return {
        "count": len(vals),
        "p50": pct(vals, 50),
        "p95": pct(vals, 95),
        "p99": pct(vals, 99),
        "max": vals[-1] if vals else None,
    }


def wait_payload(client, topic, event, client_msg_id, timeout):
    """Wait for a channel event whose payload carries our client_msg_id.

    Events can arrive in any order (broadcast vs ack) and presence_state /
    presence_diff pushes interleave freely; match on client_msg_id instead
    of assuming arrival order.
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            raw = ws_recv_text(client.sock, deadline)
        except WsError:
            break
        try:
            msg = json.loads(raw)
        except json.JSONDecodeError:
            continue
        if not isinstance(msg, list) or len(msg) != 5:
            continue
        if msg[2] != topic or msg[3] != event:
            continue
        payload = msg[4]
        if isinstance(payload, dict) and payload.get("client_msg_id") == client_msg_id:
            return payload
    raise WsError(f"timed out waiting for {event} with client_msg_id {client_msg_id}")


class Worker(threading.Thread):
    def __init__(self, wid, args, results, lock):
        super().__init__(daemon=True)
        self.wid = wid
        self.args = args
        self.results = results
        self.lock = lock

    def run(self):
        a = self.args
        run_tag = secrets.token_hex(4)
        topic = f"room:bench-{run_tag}-{self.wid}"
        try:
            client = PhoenixClient(a.host, a.port, a.token)
        except Exception as e:  # noqa: BLE001 - count, don't crash the run
            with self.lock:
                self.results["errors"] += 1
                self.results["error_kinds"]["connect"] += 1
            return
        try:
            t0 = time.perf_counter()
            client.join(topic, {"device_id": f"bench-{self.wid}"}, timeout=20)
            join_ms = (time.perf_counter() - t0) * 1000.0
            with self.lock:
                self.results["conns_ok"] += 1
                self.results["join_ms"].append(join_ms)

            for _ in range(a.msgs):
                cmid = secrets.token_hex(16)
                start = time.perf_counter()
                client.push(topic, "message:new",
                            {"body": a.body, "client_msg_id": cmid})
                wait_payload(client, topic, "message:new", cmid, timeout=20)
                wait_payload(client, topic, "msg:ack", cmid, timeout=20)
                lat_ms = (time.perf_counter() - start) * 1000.0
                with self.lock:
                    self.results["acks_ok"] += 1
                    self.results["ack_ms"].append(lat_ms)
        except Exception:  # noqa: BLE001 - partial worker results are still data
            with self.lock:
                self.results["errors"] += 1
                self.results["error_kinds"]["send_or_ack"] += 1
        finally:
            try:
                client.close()
            except Exception:  # noqa: BLE001
                pass


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("host")
    ap.add_argument("port", type=int)
    ap.add_argument("--conns", type=int, default=50,
                    help="concurrent websocket connections (default 50)")
    ap.add_argument("--msgs", type=int, default=20,
                    help="messages per connection (default 20)")
    ap.add_argument("--body-bytes", type=int, default=256,
                    help="message body size in bytes (default 256)")
    ap.add_argument("--token", default="bench-token",
                    help="socket token param (default bench-token)")
    ap.add_argument("--ramp", type=float, default=0.0,
                    help="seconds to spread connection starts over (default 0)")
    ap.add_argument("--fail-on-errors", action="store_true",
                    help="exit 3 if any connection failed or any ack was lost")
    args = ap.parse_args()
    if args.conns <= 0 or args.msgs <= 0:
        ap.error("--conns and --msgs must be positive")

    args.body = "x" * args.body_bytes

    results = {
        "conns_ok": 0,
        "join_ms": [],
        "acks_ok": 0,
        "ack_ms": [],
        "errors": 0,
        "error_kinds": {"connect": 0, "send_or_ack": 0},
    }
    lock = threading.Lock()

    t_start = time.perf_counter()
    workers = [Worker(i, args, results, lock) for i in range(args.conns)]
    for i, w in enumerate(workers):
        if args.ramp > 0 and i > 0:
            time.sleep(args.ramp / args.conns)
        w.start()
    for w in workers:
        w.join()
    duration_s = time.perf_counter() - t_start

    msgs_target = args.conns * args.msgs
    summary = {
        "target": f"{args.host}:{args.port}",
        "params": {"conns": args.conns, "msgs_per_conn": args.msgs,
                   "body_bytes": args.body_bytes, "ramp_s": args.ramp},
        "conns_target": args.conns,
        "conns_ok": results["conns_ok"],
        "join_latency_ms": summarize(results["join_ms"]),
        "msgs_target": msgs_target,
        "acks_ok": results["acks_ok"],
        "errors": results["errors"],
        "error_kinds": results["error_kinds"],
        "duration_s": round(duration_s, 3),
        "conns_per_s": round(results["conns_ok"] / duration_s, 1),
        "msgs_per_s": round(results["acks_ok"] / duration_s, 1),
        "ack_latency_ms": summarize(results["ack_ms"]),
        "notes": ("acks_ok < msgs_target or errors > 0 means the SERVICE "
                  "dropped messages/acks under load — investigate, do not "
                  "re-run and average it away."),
    }
    print(json.dumps(summary, indent=2))
    if args.fail_on_errors and (summary["errors"] > 0 or
                                summary["acks_ok"] != summary["msgs_target"]):
        print("bench_ws: --fail-on-errors: dropped messages or failed "
              "connections (see summary)", file=sys.stderr)
        sys.exit(3)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # noqa: BLE001 - harness-level failure
        print(f"bench_ws FAILED: {e}", file=sys.stderr)
        sys.exit(1)
