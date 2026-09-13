.PHONY: test test-minecraft-controller test-moneyforwardme-to-bigquery \
        test-infra test-infra-minecraft test-infra-moneyforwardme-to-bigquery \
        test-minecraft test-moneyforward test-infra-mf test-infra-core help

help:
	@echo "Available targets (Module-based):"
	@echo "  make test                                  - Run all tests across all modules and infrastructure"
	@echo "  make test-minecraft-controller             - Run Minecraft Discord Bot module tests (Go)"
	@echo "  make test-moneyforwardme-to-bigquery        - Run MoneyForward ME module tests (Python & Shell)"
	@echo "  make test-infra                            - Run all Terraform infrastructure tests"
	@echo "  make test-infra-minecraft                  - Run Minecraft & Core infrastructure tests"
	@echo "  make test-infra-moneyforwardme-to-bigquery - Run MoneyForward ME infrastructure tests"

# ------------------------------------------------------------------------------
# All Modules & Infrastructure
# ------------------------------------------------------------------------------
test: test-minecraft-controller test-moneyforwardme-to-bigquery test-infra

# ------------------------------------------------------------------------------
# Module: Minecraft Controller (Go Bot)
# ------------------------------------------------------------------------------
test-minecraft-controller:
	@echo "▶ Running Minecraft Controller (Go) tests..."
	go test -v ./apps/minecraft-controller

test-minecraft: test-minecraft-controller

# ------------------------------------------------------------------------------
# Module: MoneyForward ME to BigQuery (Python & Shell)
# ------------------------------------------------------------------------------
test-moneyforwardme-to-bigquery: test-moneyforwardme-to-bigquery-python test-moneyforwardme-to-bigquery-shell

test-moneyforwardme-to-bigquery-python:
	@echo "▶ Running MoneyForward ME Python specification tests..."
	cd apps/moneyforwardme-to-bigquery && uv run --extra dev pytest -v

test-moneyforwardme-to-bigquery-shell:
	@echo "▶ Running MoneyForward ME Shell script specification tests..."
	bash apps/moneyforwardme-to-bigquery/tests/trigger-moneyforwardme-to-bigquery_test.sh
	bash apps/moneyforwardme-to-bigquery/tests/entrypoint_test.sh

test-moneyforward: test-moneyforwardme-to-bigquery

# ------------------------------------------------------------------------------
# Module: Infrastructure (Terraform)
# ------------------------------------------------------------------------------
test-infra:
	@echo "▶ Running Terraform Quality Gate (All)..."
	./infrastructure/scripts/test-terraform.sh

test-infra-minecraft:
	@echo "▶ Running Minecraft & Core Infrastructure tests (main_test.tftest.hcl)..."
	./infrastructure/scripts/test-terraform.sh -filter=main_test.tftest.hcl

test-infra-core: test-infra-minecraft

test-infra-moneyforwardme-to-bigquery:
	@echo "▶ Running MoneyForward ME Infrastructure tests (moneyforwardme-to-bigquery_test.tftest.hcl)..."
	./infrastructure/scripts/test-terraform.sh -filter=moneyforwardme-to-bigquery_test.tftest.hcl

test-infra-mf: test-infra-moneyforwardme-to-bigquery


