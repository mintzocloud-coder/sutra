#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
"""OpenAPI single-source-of-truth coverage gate (G4 part 1).

Asserts that docs/openapi.yaml documents EXACTLY what the Go API serves:

1. Every route registered in services/api/cmd/api/main.go has a matching
   path in the spec (code is never ahead of the docs).
2. Every documented method of a route has a matching operation in the spec
   (method-level coverage where the router declares methods statically).
3. Every spec path is served by the code (the docs never describe a route
   that does not exist).
4. Internal routes (e.g. /internal/livekit/webhook) are NOT in the public
   spec.
5. The /v1/ai reverse-proxy prefix is documented via its concrete
   sub-paths (the proxy itself is a passthrough; the spec holds the AI
   service contract).

Route methods come from methodGuard([]string{...}) on the registration
line. Handlers that dispatch methods internally (FollowHandler,
CartHandler, OrdersHandler, RoomsHandler, the ai proxy, /healthz,
/readyz) are checked at path level only and reported as such.

The /v1/mini-apps/{id}/{action} pattern expands via the AppMux dispatch
in services/api/internal/miniapp/handler.go (parsed, not hardcoded, so
the expansion cannot rot).

Exit 0 when the spec is a faithful single source of truth, 1 otherwise.
Run in CI (the `openapi` job) and before every API change ships.
"""
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MAIN_GO = os.path.join(REPO, "services/api/cmd/api/main.go")
MINIAPP_HANDLER = os.path.join(REPO, "services/api/internal/miniapp/handler.go")
SPEC = os.path.join(REPO, "docs/openapi.yaml")

# Registered but deliberately undocumented: not part of the public API.
INTERNAL_ROUTES = {"/internal/livekit/webhook"}
# Reverse-proxy prefixes: the spec documents concrete sub-paths instead.
PROXY_PREFIXES = {"/v1/ai"}
# Mux catch-alls that are not API routes.
SKIP_PATTERNS = {"/", "/v1/"}

HTTP_METHODS = {"get", "post", "put", "patch", "delete", "options", "head"}


def fail(msg):
    print(f"COVERAGE-FAIL: {msg}")
    return False


def parse_spec_paths(path):
    """Minimal YAML parse of the paths: block (no dependency)."""
    paths = {}
    in_paths = False
    current = None
    with open(path) as f:
        for line in f:
            stripped = line.rstrip("\n")
            if re.match(r"^[A-Za-z]", stripped):
                in_paths = stripped.startswith("paths:")
                current = None
                continue
            if not in_paths:
                continue
            m = re.match(r"^  (/[^:\s][^:]*):\s*(#.*)?$", stripped)
            if m:
                current = m.group(1)
                paths[current] = set()
                continue
            m = re.match(r"^    ([a-z]+):\s*(#.*)?$", stripped)
            if m and current and m.group(1) in HTTP_METHODS:
                paths[current].add(m.group(1))
    return paths


def parse_code_routes(main_go):
    """Return list of (pattern, methods|None). methods=None => dispatched."""
    routes = []
    with open(main_go) as f:
        for lineno, line in enumerate(f, 1):
            for reg in ("v1.Handle", "mux.Handle", "mux.HandleFunc"):
                m = re.search(reg + r'\(\s*"([^"]+)"', line)
                if not m:
                    continue
                pattern = m.group(1)
                gm = re.search(r"methodGuard\(\[\]string\{([^{}]*)\}", line)
                if gm:
                    methods = {
                        x.group(1).lower()
                        for x in re.finditer(r"http\.Method(\w+)", gm.group(1))
                    }
                    methods = {x for x in methods if x in HTTP_METHODS}
                else:
                    methods = None
                routes.append((pattern, methods, lineno))
    return routes


def parse_miniapp_actions(handler_path):
    """Parse AppMux `case action == "x" && r.Method == http.MethodY ...`."""
    actions = {}
    rx = re.compile(
        r'case action == "(\w+)"'
        r"(?:\s*&&\s*\(?r\.Method == http\.Method(\w+)"
        r"(?:\s*\|\|\s*r\.Method == http\.Method(\w+))?\)?)?"
    )
    with open(handler_path) as f:
        for line in f:
            m = rx.search(line)
            if m:
                methods = {x.lower() for x in m.groups()[1:] if x}
                actions[m.group(1)] = methods or None
    return actions


def main():
    ok = True
    spec_paths = parse_spec_paths(SPEC)
    code_routes = parse_code_routes(MAIN_GO)
    miniapp_actions = parse_miniapp_actions(MINIAPP_HANDLER)

    if not miniapp_actions:
        ok = fail("no mini-app actions parsed from AppMux — dispatch changed?")
    else:
        print(f"mini-app actions from AppMux: {sorted(miniapp_actions)}")

    # Expand code routes into concrete (path, methods) expectations.
    expanded = []  # (path, methods|None, source)
    for pattern, methods, lineno in code_routes:
        if pattern in SKIP_PATTERNS:
            continue
        if pattern in INTERNAL_ROUTES:
            continue
        if pattern in PROXY_PREFIXES or any(
            pattern == p + "/" for p in PROXY_PREFIXES
        ):
            continue  # covered by the proxy-prefix assertion below
        if "{action}" in pattern:
            for action, amethods in miniapp_actions.items():
                expanded.append(
                    (pattern.replace("{action}", action), amethods,
                     f"main.go:{lineno} AppMux action={action}")
                )
            continue
        expanded.append((pattern, methods, f"main.go:{lineno}"))

    # 1+2: code -> spec.
    dispatched = []
    for path, methods, source in expanded:
        if path not in spec_paths:
            ok = fail(f"missing in spec: {path} (registered at {source})")
            continue
        if methods is None:
            dispatched.append(path)
        else:
            for meth in sorted(methods):
                if meth not in spec_paths[path]:
                    ok = fail(
                        f"method missing in spec: {meth.upper()} {path} "
                        f"(registered at {source})"
                    )

    # 3: spec -> code (no stale docs).
    concrete_code = {p for p, _, _ in expanded}
    for spath in spec_paths:
        if any(spath == p or spath.startswith(p + "/") for p in PROXY_PREFIXES):
            continue
        if spath not in concrete_code:
            ok = fail(f"stale in spec (no code route): {spath}")

    # 4: internal routes stay out of the public spec.
    for internal in INTERNAL_ROUTES:
        if internal in spec_paths:
            ok = fail(f"internal route leaked into public spec: {internal}")
    # ...and they must still be registered (the gate knows about them).
    registered = {p for p, _, _ in parse_code_routes(MAIN_GO)}
    for internal in INTERNAL_ROUTES:
        if internal not in registered:
            ok = fail(f"internal route no longer registered: {internal}")

    # 5: proxy prefixes have documented sub-paths.
    for prefix in PROXY_PREFIXES:
        sub = [p for p in spec_paths if p.startswith(prefix + "/")]
        if not sub:
            ok = fail(f"proxy prefix {prefix} has no documented sub-paths")

    print(f"spec paths: {len(spec_paths)}, code routes expanded: {len(expanded)}")
    if dispatched:
        print(f"path-level only (dispatched handlers): {sorted(set(dispatched))}")
    if ok:
        print("OPENAPI COVERAGE OK: spec is a faithful single source of truth")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
