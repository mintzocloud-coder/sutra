# SPDX-License-Identifier: AGPL-3.0-or-later
import Config

# Production config, compiled into the `mix release` image (B7).
#
# This file is intentionally minimal: everything environment-specific or
# secret lives in config/runtime.exs, which runs at BOOT (not at build) and
# fails closed when required env (DATABASE_URL, SECRET_KEY_BASE) is missing.
# Nothing here may contain a secret — the image is built once and shipped
# to every environment.

config :logger, level: :info
