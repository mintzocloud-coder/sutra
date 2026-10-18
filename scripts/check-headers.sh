#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
# Fail if any source file lacks the AGPLv3 SPDX header.
# Skips: LICENSE, NOTICE, .md docs, configs, vendored dirs, .git,
# generated checksum/lock files (go.sum, *.lock).
set -euo pipefail

HEADER="SPDX-License-Identifier: AGPL-3.0-or-later"
FAIL=0

while IFS= read -r f; do
  if ! head -n 5 "$f" | grep -q "$HEADER"; then
    echo "MISSING HEADER: $f"
    FAIL=1
  fi
done < <(git ls-files \
  | grep -vE '^LICENSE$|^NOTICE$|\.md$|\.json$|\.ya?ml$|\.toml$|\.lock$|\.sum$|^\.github/' \
  | grep -vE '^\.git/|\.pytest_cache/')

if [ "$FAIL" -eq 1 ]; then
  echo ""
  echo "Add this as the first line of each flagged file:"
  echo "// SPDX-License-Identifier: AGPL-3.0-or-later   (use # for scripts, -- for SQL)"
  exit 1
fi
echo "All source files carry the SPDX header."
