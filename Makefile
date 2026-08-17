.PHONY: test test-bot test-infra help

help:
	@echo "Available targets:"
	@echo "  make test        - Run both Go Discord Bot tests and Terraform tests"
	@echo "  make test-bot    - Run Go Discord Bot unit & mock tests"
	@echo "  make test-infra  - Run Terraform Quality Gate via safe rootless Podman"

test: test-bot test-infra

test-bot:
	@echo "▶ Running Go Discord Bot unit tests..."
	go test -v ./apps/minecraft-controller

test-infra:
	@echo "▶ Running Terraform Quality Gate..."
	./infrastructure/scripts/test-terraform.sh
