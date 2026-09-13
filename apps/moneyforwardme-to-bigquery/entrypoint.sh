#!/usr/bin/env bash
# ==============================================================================
# Instructions for AI agents:
# Bootstrap entrypoint for Cloud Run container execution.
# Installs uv and launches sync.py.
# ==============================================================================
set -euo pipefail

main() {
  local repo="${GITHUB_REPOSITORY:-mintommm/googlecloud-alwaysfree}"
  local branch="${GITHUB_BRANCH:-main}"
  local app_dir="${APP_DIR:-/tmp/app}"

  mkdir -p "${app_dir}"

  export PATH="${HOME}/.local/bin:${HOME}/.cargo/bin:${PATH}"

  if ! command -v uv &> /dev/null; then
    # Dynamic installation avoids consuming Artifact Registry storage
    curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR="${HOME}/.local/bin" sh
  fi

  curl -sSL "https://raw.githubusercontent.com/${repo}/${branch}/apps/moneyforwardme-to-bigquery/sync.py" -o "${app_dir}/sync.py"

  # Ensure Playwright Chromium browser is installed
  uv run --with playwright playwright install chromium

  exec uv run "${app_dir}/sync.py"
}

main "$@"
