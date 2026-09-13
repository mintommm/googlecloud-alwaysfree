#!/usr/bin/env bash
# ==============================================================================
# Instructions for AI agents:
# Bootstrap entrypoint for Cloud Run container execution.
# Installs uv and launches sync.py.
# ==============================================================================
set -euo pipefail

main() {
  local script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  local target_py="${script_dir}/sync.py"

  export PATH="${HOME}/.local/bin:${HOME}/.cargo/bin:${PATH}"

  if ! command -v uv &> /dev/null; then
    # Dynamic installation avoids consuming Artifact Registry storage
    curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR="${HOME}/.local/bin" sh
  fi

  if [[ ! -f "${target_py}" ]]; then
    local repo="${GITHUB_REPOSITORY:-mintommm/googlecloud-alwaysfree}"
    local branch="${GITHUB_BRANCH:-main}"
    local app_dir="${APP_DIR:-/tmp/app}"
    mkdir -p "${app_dir}"
    target_py="${app_dir}/sync.py"
    curl -sSL "https://raw.githubusercontent.com/${repo}/${branch}/apps/moneyforwardme-to-bigquery/sync.py" -o "${target_py}"
  fi

  # Ensure Playwright Chromium browser is installed
  uv run --with playwright playwright install chromium

  exec uv run "${target_py}"
}

main "$@"
