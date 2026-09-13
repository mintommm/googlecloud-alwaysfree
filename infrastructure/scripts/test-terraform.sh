#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INFRA_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# 安全な公式イメージとバージョンの完全固定 (タイポスクワッティング・偽イメージ防止)
TF_IMAGE="docker.io/hashicorp/terraform:1.10.4"

echo "========================================================"
echo " Running Terraform Quality Gate via Rootless Podman"
echo " Image:     ${TF_IMAGE}"
echo " Directory: ${INFRA_DIR}"
echo "========================================================"

if command -v podman &> /dev/null; then
    echo "▶ 1. Checking HCL2 format (fmt -check)..."
    podman run --rm -v "${INFRA_DIR}:/workspace:ro" -w /workspace "${TF_IMAGE}" fmt -check

    echo "▶ 2. Initializing stateless provider schema (init -backend=false)..."
    podman run --rm -v "${INFRA_DIR}:/workspace" -w /workspace "${TF_IMAGE}" init -backend=false -no-color

    echo "▶ 3. Validating provider schema & syntax (validate)..."
    podman run --rm -v "${INFRA_DIR}:/workspace" -w /workspace "${TF_IMAGE}" validate -no-color

    echo "▶ 4. Running native unit assertions (test)..."
    podman run --rm -v "${INFRA_DIR}:/workspace" -w /workspace "${TF_IMAGE}" test -no-color "$@"
elif command -v terraform &> /dev/null || command -v terraform.exe &> /dev/null; then
    TF_CMD="terraform"
    if ! command -v terraform &> /dev/null; then
        TF_CMD="terraform.exe"
    fi
    echo "ℹ️ Podman not found; running via host native Terraform CLI (${TF_CMD})."
    cd "${INFRA_DIR}"

    echo "▶ 1. Checking HCL2 format (fmt -check)..."
    "${TF_CMD}" fmt -check

    echo "▶ 2. Initializing stateless provider schema (init -backend=false)..."
    "${TF_CMD}" init -backend=false -no-color

    echo "▶ 3. Validating provider schema & syntax (validate)..."
    "${TF_CMD}" validate -no-color

    echo "▶ 4. Running native unit assertions (test)..."
    "${TF_CMD}" test -no-color "$@"
else
    echo "❌ Error: Neither podman nor terraform is installed on this host."
    exit 1
fi

echo "========================================================"
echo "✅ All Terraform Tests PASSED Successfully!"
echo "========================================================"
