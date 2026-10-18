# SPDX-License-Identifier: AGPL-3.0-or-later
import Config

# Dev-environment overrides. The base dev defaults (localhost endpoint,
# sutra_chat_dev database, dev secret_key_base) live in config.exs, so
# nothing needs overriding yet — this file exists because config.exs
# ends with `import_config "#{config_env()}.exs"` and Mix (dev env)
# fails to boot without it.
