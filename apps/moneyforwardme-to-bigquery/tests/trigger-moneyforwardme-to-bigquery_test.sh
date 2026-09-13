#!/usr/bin/env bash
# ==============================================================================
# Instructions for AI agents:
# This script executes specification tests for trigger-moneyforwardme-to-bigquery.sh.
# It runs without any external dependencies or GCP network connection.
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET_SCRIPT="${SCRIPT_DIR}/../trigger-moneyforwardme-to-bigquery.sh"

echo "=== Running Specification Tests for trigger-moneyforwardme-to-bigquery.sh ==="

source "${TARGET_SCRIPT}"

test_valid_month_format() {
  echo -n "Testing valid month format (2024-05)... "
  validate_month "2024-05"
  echo "PASS"
}

test_invalid_month_format_single_digit() {
  echo -n "Testing invalid month format (2024-5)... "
  if validate_month "2024-5" 2>/dev/null; then
    echo "FAIL: Expected failure on '2024-5'"
    exit 1
  fi
  echo "PASS"
}

test_invalid_month_format_string() {
  echo -n "Testing invalid month format ('invalid')... "
  if validate_month "invalid" 2>/dev/null; then
    echo "FAIL: Expected failure on 'invalid'"
    exit 1
  fi
  echo "PASS"
}

test_http_200_handling() {
  echo -n "Testing HTTP 200 response handling... "
  local output
  output=$(
    curl() {
      if [[ "$*" == *"/identity"* ]]; then
        echo "dummy_token"
      else
        echo '{"status":"success"}' > "$3"
        echo "200"
      fi
    }
    SERVICE_URL="http://dummy" main
  )
  if [[ "${output}" != *"Sync job finished successfully."* ]]; then
    echo "FAIL: Expected success message, got: ${output}"
    exit 1
  fi
  echo "PASS"
}

test_http_401_handling() {
  echo -n "Testing HTTP 401 response handling... "
  local output
  local status=0
  output=$(
    curl() {
      if [[ "$*" == *"/identity"* ]]; then
        echo "dummy_token"
      else
        echo '{"status":"error\",\"error\":\"session_expired"}' > "$3"
        echo "401"
      fi
    }
    SERVICE_URL="http://dummy" main 2>&1
  ) || status=$?
  if [ "${status}" -eq 0 ]; then
    echo "FAIL: Expected non-zero exit code on 401, but got 0."
    exit 1
  fi
  if [[ "${output}" != *"Requires human re-authentication"* ]]; then
    echo "FAIL: Expected re-authentication guidance, got: ${output}"
    exit 1
  fi
  echo "PASS"
}

test_http_500_handling() {
  echo -n "Testing HTTP 500 response handling... "
  local output
  local status=0
  output=$(
    curl() {
      if [[ "$*" == *"/identity"* ]]; then
        echo "dummy_token"
      else
        echo '{"status":"error"}' > "$3"
        echo "500"
      fi
    }
    SERVICE_URL="http://dummy" main 2>&1
  ) || status=$?
  if [ "${status}" -eq 0 ]; then
    echo "FAIL: Expected non-zero exit code on 500, but got 0."
    exit 1
  fi
  if [[ "${output}" != *"Sync job failed (HTTP 500)"* ]]; then
    echo "FAIL: Expected failure message, got: ${output}"
    exit 1
  fi
  echo "PASS"
}

test_payload_generation_default() {
  echo -n "Testing default payload generation (empty)... "
  local tmp_payload
  tmp_payload=$(mktemp)
  trap "rm -f '${tmp_payload}'" RETURN
  curl() {
    if [[ "$*" == *"/identity"* ]]; then
      echo "dummy_token"
    else
      while [ $# -gt 0 ]; do
        if [ "$1" = "-d" ]; then
          echo "$2" > "${tmp_payload}"
          shift 2
        else
          shift
        fi
      done
      echo "200"
    fi
  }
  SERVICE_URL="http://dummy" main >/dev/null
  local actual
  actual=$(cat "${tmp_payload}")
  if [ "${actual}" != "{}" ]; then
    echo "FAIL: Expected '{}', got: ${actual}"
    exit 1
  fi
  echo "PASS"
}

test_payload_generation_single_month() {
  echo -n "Testing single month payload generation... "
  local tmp_payload
  tmp_payload=$(mktemp)
  trap "rm -f '${tmp_payload}'" RETURN
  curl() {
    if [[ "$*" == *"/identity"* ]]; then
      echo "dummy_token"
    else
      while [ $# -gt 0 ]; do
        if [ "$1" = "-d" ]; then
          echo "$2" > "${tmp_payload}"
          shift 2
        else
          shift
        fi
      done
      echo "200"
    fi
  }
  SERVICE_URL="http://dummy" main "2024-05" >/dev/null
  local actual
  actual=$(cat "${tmp_payload}")
  if [ "${actual}" != '{"month":"2024-05"}' ]; then
    echo "FAIL: Expected '{\"month\":\"2024-05\"}', got: ${actual}"
    exit 1
  fi
  echo "PASS"
}

test_payload_generation_date_range() {
  echo -n "Testing date range payload generation... "
  local tmp_payload
  tmp_payload=$(mktemp)
  trap "rm -f '${tmp_payload}'" RETURN
  curl() {
    if [[ "$*" == *"/identity"* ]]; then
      echo "dummy_token"
    else
      while [ $# -gt 0 ]; do
        if [ "$1" = "-d" ]; then
          echo "$2" > "${tmp_payload}"
          shift 2
        else
          shift
        fi
      done
      echo "200"
    fi
  }
  SERVICE_URL="http://dummy" main "2024-01" "2024-12" >/dev/null
  local actual
  actual=$(cat "${tmp_payload}")
  if [ "${actual}" != '{"from":"2024-01","to":"2024-12"}' ]; then
    echo "FAIL: Expected '{\"from\":\"2024-01\",\"to\":\"2024-12\"}', got: ${actual}"
    exit 1
  fi
  echo "PASS"
}

test_invalid_second_month_format() {
  echo -n "Testing invalid second month format in range... "
  local status=0
  SERVICE_URL="http://dummy" main "2024-01" "2024-1" >/dev/null 2>&1 || status=$?
  if [ "${status}" -eq 0 ]; then
    echo "FAIL: Expected non-zero exit code on invalid second month"
    exit 1
  fi
  echo "PASS"
}

test_valid_month_format
test_invalid_month_format_single_digit
test_invalid_month_format_string
test_invalid_second_month_format
test_payload_generation_default
test_payload_generation_single_month
test_payload_generation_date_range
test_http_200_handling
test_http_401_handling
test_http_500_handling

echo "All specification tests for trigger script passed successfully!"

