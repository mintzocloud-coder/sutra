# SPDX-License-Identifier: AGPL-3.0-or-later
defmodule SutraChat.Presence do
  @moduledoc "CRDT-based presence tracking (Phoenix.Presence)."
  use Phoenix.Presence,
    otp_app: :sutra_chat,
    pubsub_server: SutraChat.PubSub
end
