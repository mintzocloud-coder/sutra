# SPDX-License-Identifier: AGPL-3.0-or-later
"""Eval harness skeleton for the Sutra personal-AI chat (E6).

Runs the golden prompt set in eval/golden/*.yaml against the public
API surface (POST {SUTRA_API_BASE}/v1/ai/chat), parses the AG-UI SSE
stream, and checks keyword assertions. Results land in
eval/results/<utc-timestamp>.json (gitignored — see .gitignore).

Usage:
  EVAL_TOKEN='<valid JWT>' EVAL_BACKEND=stub \
    python3 run_eval.py --api-base http://localhost:8080
  python3 run_eval.py --dry-run        # validate golden files without calling anything

This is a SKELETON: keyword presence proves the plumbing (auth ->
proxy -> graph -> stream), not reply quality. It must never block CI
until the real-model golden set is curated (Wave 2). Exit code is 0
when every included case passes, 1 otherwise.

Env:
  SUTRA_API_BASE  base URL of the Go API (default http://localhost:8080)
  EVAL_TOKEN      Bearer <redacted> for the Go API (a real Hydra-issued JWT, or
                  the dev bypass subject's flow — never commit it)
  EVAL_BACKEND    "stub" (AI_MODEL_STUB=1) or "real"; filters cases by
                  their backends list (default: stub)
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

try:
    import yaml
except ImportError:  # pragma: no cover - the ai image pins pyyaml
    sys.exit("pyyaml is required (pip install -r services/ai/requirements.txt)")

HERE = Path(__file__).resolve().parent
GOLDEN_DIR = HERE / "golden"
RESULTS_DIR = HERE / "results"

AGUI_TYPES_WITH_DELTA = {"TEXT_MESSAGE_CONTENT"}


def _sse_text_events(body: bytes) -> list[dict]:
    """Parse SSE data frames into JSON event dicts."""
    events = []
    for frame in body.decode("utf-8", "replace").split("\n\n"):
        for line in frame.splitlines():
            if line.startswith("data:"):
                try:
                    events.append(json.loads(line[5:].strip()))
                except json.JSONDecodeError:
                    pass
    return events


def _post_chat(api_base: str, bearer: str, payload: dict) -> tuple[int, bytes, dict]:
    req = urllib.request.Request(
        api_base.rstrip("/") + "/v1/ai/chat",
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {bearer}",
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return resp.status, resp.read(), dict(resp.headers)
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(), dict(exc.headers)


def _run_case(api_base: str, bearer: str, case: dict, thread_id: str | None) -> dict:
    """Run one golden case; returns a result dict (no exceptions escape)."""
    name = case["name"]
    started = time.monotonic()
    try:
        if case.get("send_empty"):
            status, body, _ = _post_chat(api_base, bearer, {"messages": []})
            expected = case.get("expect_status", 422)
            ok = status == expected
            detail = f"status={status} (expected {expected})"
            return _result(name, ok, detail, started, thread_id)

        assistant_text = ""
        for turn in case.get("turns", []):
            payload = {
                "messages": [{"role": "user", "content": turn["user"]}],
            }
            if thread_id:
                payload["thread_id"] = thread_id
            status, body, _ = _post_chat(api_base, bearer, payload)
            if status != 200:
                return _result(name, False, f"turn failed: HTTP {status}", started, thread_id)
            events = _sse_text_events(body)
            seen_types = {e.get("type") for e in events}
            if "RUN_STARTED" not in seen_types or "RUN_FINISHED" not in seen_types:
                return _result(name, False,
                               f"incomplete AG-UI stream: {sorted(seen_types)}",
                               started, thread_id)
            if not thread_id:
                run_started = next(e for e in events if e.get("type") == "RUN_STARTED")
                thread_id = run_started.get("threadId")
            assistant_text += "".join(
                e.get("delta", "") for e in events
                if e.get("type") in AGUI_TYPES_WITH_DELTA
            )

        lowered = assistant_text.lower()
        missing = [k for k in case.get("expect_all", []) if k.lower() not in lowered]
        present_bad = [k for k in case.get("expect_absent", []) if k.lower() in lowered]
        ok = not missing and not present_bad
        detail = f"{len(assistant_text)} assistant chars"
        if missing:
            detail += f"; missing={missing}"
        if present_bad:
            detail += f"; leaked={present_bad}"
        return _result(name, ok, detail, started, thread_id)
    except Exception as exc:  # skeleton: report, don't crash the run
        return _result(name, False, f"exception: {type(exc).__name__}: {exc}",
                       started, thread_id)


def _result(name: str, ok: bool, detail: str, started: float,
            thread_id: str | None) -> dict:
    return {
        "name": name,
        "passed": ok,
        "detail": detail,
        "duration_s": round(time.monotonic() - started, 2),
        "thread_id": thread_id,
    }


def _load_cases(backend: str) -> list[dict]:
    cases = []
    for path in sorted(GOLDEN_DIR.glob("*.yaml")):
        with open(path, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
        for case in doc.get("cases", []):
            allowed = case.get("backends")
            if allowed and backend not in allowed:
                continue
            cases.append(case)
    return cases


def main() -> int:
    ap = argparse.ArgumentParser(description="Sutra AI eval harness skeleton")
    ap.add_argument("--api-base", default=os.environ.get("SUTRA_API_BASE",
                                                         "http://localhost:8080"))
    ap.add_argument("--dry-run", action="store_true",
                    help="validate golden files only, no HTTP calls")
    args = ap.parse_args()

    backend = os.environ.get("EVAL_BACKEND", "stub")
    cases = _load_cases(backend)
    if not cases:
        print(f"no cases for backend={backend!r}; nothing to do")
        return 0

    if args.dry_run:
        for case in cases:
            assert case.get("name"), f"case without name in golden set"
            assert case.get("turns") or case.get("send_empty"), \
                f"case {case['name']!r}: needs turns or send_empty"
        print(f"dry-run OK: {len(cases)} case(s) for backend={backend!r}")
        return 0

    bearer = os.environ.get("EVAL_TOKEN", "").strip()
    if not bearer:
        sys.exit("EVAL_TOKEN is required (a valid JWT for the Go API)")

    results = []
    for case in cases:
        res = _run_case(args.api_base, bearer, case, None)
        results.append(res)
        mark = "PASS" if res["passed"] else "FAIL"
        print(f"[{mark}] {res['name']}: {res['detail']}")

    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    out_path = RESULTS_DIR / f"{stamp}-{backend}.json"
    with open(out_path, "w", encoding="utf-8") as fh:
        json.dump({
            "backend": backend,
            "api_base": args.api_base,
            "golden_version": "v1",
            "ran_at_utc": stamp,
            "passed": sum(1 for r in results if r["passed"]),
            "failed": sum(1 for r in results if not r["passed"]),
            "cases": results,
        }, fh, indent=2)
    print(f"results -> {out_path}")

    return 0 if all(r["passed"] for r in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
