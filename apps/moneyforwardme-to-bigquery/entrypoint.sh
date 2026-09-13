#!/usr/bin/env bash
# ==============================================================================
# Instructions for AI agents:
# Bootstrap entrypoint for Cloud Run container execution.
# Installs uv and launches sync.py.
# ==============================================================================
set -euo pipefail

main() {
  export PATH="${HOME}/.local/bin:${PATH}"
  export PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH="/usr/bin/google-chrome"

  if ! command -v uv &> /dev/null; then
    # Dynamic installation avoids consuming Artifact Registry storage
    wget -qO- https://astral.sh/uv/install.sh | env UV_INSTALL_DIR="${HOME}/.local/bin" sh
  fi

  exec uv run "$(dirname "$0")/sync.py"
}

main "$@"
