#!/usr/bin/env bash
# ==============================================================================
# Instructions for AI agents:
# Specification tests for Cloud Run entrypoint.sh bootstrap script.
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET_SCRIPT="${SCRIPT_DIR}/../entrypoint.sh"

echo "=== Running Specification Tests for entrypoint.sh ==="

test_syntax_check() {
  echo -n "Testing shell script syntax (bash -n)... "
  bash -n "${TARGET_SCRIPT}"
  echo "PASS"
}

test_bootstrap_flow_with_mock_tools() {
  echo -n "Testing bootstrap flow and tool execution... "
  local tmp_dir
  tmp_dir=$(mktemp -d)
  trap "rm -rf '${tmp_dir}'" RETURN

  local curl_calls_file="${tmp_dir}/curl_calls.txt"
  local uv_calls_file="${tmp_dir}/uv_calls.txt"

  local mock_bin="${tmp_dir}/bin"
  mkdir -p "${mock_bin}"

  # Mock curl to intercept download URLs
  cat << 'EOF' > "${mock_bin}/curl"
#!/usr/bin/env bash
echo "$*" >> "${CURL_CALLS_FILE}"
for arg in "$@"; do
  if [ "${arg}" = "-o" ]; then
    touch "${@: -1}"
  fi
done
EOF
  chmod +x "${mock_bin}/curl"

  # Mock uv to intercept runner command
  cat << 'EOF' > "${mock_bin}/uv"
#!/usr/bin/env bash
echo "$*" >> "${UV_CALLS_FILE}"
exit 0
EOF
  chmod +x "${mock_bin}/uv"

  PATH="${mock_bin}:${PATH}" \
  CURL_CALLS_FILE="${curl_calls_file}" \
  UV_CALLS_FILE="${uv_calls_file}" \
  APP_DIR="${tmp_dir}/app" \
  GITHUB_REPOSITORY="custom-owner/custom-repo" \
  GITHUB_BRANCH="feature-test" \
  bash "${TARGET_SCRIPT}"

  if ! grep -q "https://raw.githubusercontent.com/custom-owner/custom-repo/feature-test/apps/moneyforwardme-to-bigquery/sync.py" "${curl_calls_file}"; then
    echo "FAIL: Expected curl to fetch sync.py from configured repository and branch"
    exit 1
  fi

  if ! grep -q "run ${tmp_dir}/app/sync.py" "${uv_calls_file}"; then
    echo "FAIL: Expected uv run to be invoked with target script path"
    exit 1
  fi

  echo "PASS"
}

test_syntax_check
test_bootstrap_flow_with_mock_tools

echo "All specification tests for entrypoint script passed successfully!"
