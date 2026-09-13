#!/usr/bin/env bash
# ==============================================================================
# Instructions for AI agents:
# This script triggers the Cloud Run service `moneyforwardme-to-bigquery`.
# Run this script directly on the GCE `always_free` instance.
#
# Usage examples:
# - Default (Daily cron execution - fetches latest 3 months: current, previous, 2 months ago):
#   ./trigger-moneyforwardme-to-bigquery.sh
#
# - Backfill a specific single month (format: YYYY-MM):
#   ./trigger-moneyforwardme-to-bigquery.sh 2024-05
#
# - Backfill a date range (format: YYYY-MM YYYY-MM):
#   ./trigger-moneyforwardme-to-bigquery.sh 2024-01 2024-12
# ==============================================================================
set -euo pipefail

validate_month() {
  if ! [[ "$1" =~ ^[0-9]{4}-(0[1-9]|1[0-2])$ ]]; then
    echo "Error: Invalid month format '$1'. Expected YYYY-MM." >&2
    return 1
  fi
}

main() {
  local region="${REGION:-asia-northeast1}"
  local service_name="${SERVICE_NAME:-moneyforwardme-to-bigquery}"
  local service_url="${SERVICE_URL:-$(gcloud run services describe "${service_name}" --region="${region}" --format="value(status.url)")}"

  local payload="{}"
  if [ $# -eq 1 ]; then
    validate_month "$1" || return 1
    payload="{\"month\":\"$1\"}"
  elif [ $# -ge 2 ]; then
    validate_month "$1" || return 1
    validate_month "$2" || return 1
    payload="{\"from\":\"$1\",\"to\":\"$2\"}"
  fi

  echo "[$(date -Iseconds)] Triggering moneyforwardme-to-bigquery job... (Payload: ${payload})"

  local tmp_resp
  tmp_resp=$(mktemp)
  trap "rm -f '${tmp_resp}'" EXIT

  # Cloud Run requires an OIDC ID token with audience matching the service URL for authenticated invocation
  local id_token
  id_token=$(curl -s "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=${service_url}" -H "Metadata-Flavor: Google")

  local http_status
  http_status=$(curl -s -o "$tmp_resp" -w "%{http_code}" -X POST "${service_url}" \
    -H "Authorization: Bearer ${id_token}" \
    -H "Content-Type: application/json" \
    -d "${payload}")

  cat "$tmp_resp"
  echo ""

  if [ "${http_status}" -eq 200 ]; then
    echo "Sync job finished successfully."
  elif [ "${http_status}" -eq 401 ]; then
    echo "Error: MoneyForward session expired (HTTP 401). Requires human re-authentication via manual_refresh_session.py." >&2
    return 1
  else
    echo "Error: Sync job failed (HTTP ${http_status})" >&2
    return 1
  fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
