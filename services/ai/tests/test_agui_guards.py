# SPDX-License-Identifier: AGPL-3.0-or-later
"""E6 unit tests: /ag-ui/run abuse guards (reused E2 budget machinery).

check_rate_limit is exercised with a fake pool — no database. The
endpoint wires it eagerly (subject required + GuardRejection mapped to
a JSON 429 envelope, exactly like /chat), which the live-service test
and smoke.sh exercise end to end.
"""
from __future__ import annotations

import sys
from contextlib import contextmanager
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from app import budget as budget_mod


class _FakeCursor:
    def __init__(self, count: int):
        self._count = count

    def execute(self, sql, params=None):  # noqa: ARG002 - SQL shape irrelevant
        pass

    def fetchone(self):
        return (self._count,)


class _FakeConn:
    def __init__(self, count: int):
        self._count = count

    def cursor(self):
        @contextmanager
        def _cm():
            yield _FakeCursor(self._count)

        return _cm()

    def commit(self):
        pass


class _FakePool:
    def __init__(self, count: int):
        self._count = count

    def connection(self):
        @contextmanager
        def _cm():
            yield _FakeConn(self._count)

        return _cm()


def test_agui_rate_limit_allows_under_cap():
    budget_mod.check_rate_limit(_FakePool(59), "u1", 60)  # must not raise


def test_agui_rate_limit_rejects_over_cap():
    with pytest.raises(budget_mod.GuardRejection) as exc_info:
        budget_mod.check_rate_limit(_FakePool(61), "u1", 60)
    err = exc_info.value
    assert err.code == "ai_rate_limited"
    assert "ag-ui" in err.message or "rate limit" in err.message
    assert err.retry_after >= 1


def test_agui_rate_limit_boundary_is_inclusive():
    # Fixed-window semantics: the Nth request at the cap is allowed, the
    # N+1th is not (check increments, then compares).
    budget_mod.check_rate_limit(_FakePool(60), "u1", 60)
    with pytest.raises(budget_mod.GuardRejection):
        budget_mod.check_rate_limit(_FakePool(61), "u1", 60)


def test_agui_rate_limit_zero_disables_without_touching_pool():
    # rpm <= 0 returns before opening a connection — the fake would
    # survive anyway, but this documents the disable hatch.
    budget_mod.check_rate_limit(_FakePool(10_000), "u1", 0)


def test_agui_rate_limit_per_user_window_keyed_by_subject():
    # Same pool, two subjects: the SQL keys (subject, window_start), so
    # each caller gets an independent window. The fake returns one
    # count for both — the assertion here is on the query contract:
    # subject is passed as the only bind parameter.
    seen = []

    class _SpyCursor(_FakeCursor):
        def execute(self, sql, params=None):
            seen.append((sql, params))
            super().execute(sql, params)

    class _SpyConn(_FakeConn):
        def cursor(self):
            @contextmanager
            def _cm():
                yield _SpyCursor(self._count)

            return _cm()

    class _SpyPool(_FakePool):
        def connection(self):
            @contextmanager
            def _cm():
                yield _SpyConn(self._count)

            return _cm()

    budget_mod.check_rate_limit(_SpyPool(1), "user-42", 60)
    insert_sql, insert_params = seen[0]
    assert "ai_rate_windows" in insert_sql
    assert insert_params == ("user-42",)
