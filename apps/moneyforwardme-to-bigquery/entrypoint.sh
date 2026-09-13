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
  local app_dir="${APP_DIR:-/app}"

  mkdir -p "${app_dir}"

  if ! command -v uv &> /dev/null; then
    # Official Playwright image lacks uv; dynamic installation avoids consuming Artifact Registry storage
    curl -LsSf https://astral.sh/uv/install.sh | sh
    export PATH="${HOME}/.local/bin:${PATH}"
  fi

  curl -sSL "https://raw.githubusercontent.com/${repo}/${branch}/apps/moneyforwardme-to-bigquery/sync.py" -o "${app_dir}/sync.py"

  exec uv run "${app_dir}/sync.py"
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
