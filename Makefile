# Common commands; CI runs the same steps (.github/workflows/ci.yml).
GO_TEST_COVERAGE_VERSION := v2.20.1
COVERAGE := go run github.com/vladopajic/go-test-coverage/v2@$(GO_TEST_COVERAGE_VERSION)
SHELL := /bin/bash
BIN := bin/bank-statement-data
export GOPRIVATE := github.com/fin-assist/*

.PHONY: build test test-names cover collect-unit collect-integration collect-api check-unit check-integration check-api cover-unit cover-integration cover-api cover-combined lint fmt run migrate image up down release-check

build: ## build the binary into bin/
	go build -trimpath -o $(BIN) ./cmd/bank-statement-data

test: test-names ## go.mod tidy, unit/integration and script tests, changelog format
	go mod tidy -diff
	go vet ./...
	go test -race -count=1 -timeout=8m ./...
	python3 -m unittest discover -s scripts -p 'test_*.py'
	python3 scripts/changelog.py check

release-check: ## VERSION=vX.Y.Z: validate the CHANGELOG section before tagging
	@test -n "$(VERSION)" || { echo "usage: make release-check VERSION=vX.Y.Z"; exit 2; }
	python3 scripts/changelog.py check
	test "$$(python3 scripts/changelog.py latest)" = "$(VERSION)"

test-names: ## reject tests, fuzz targets and examples omitted from the three selectors
	go test -race -list '^(Test|Fuzz|Example)' -timeout=8m ./... > test-names.txt
	@if grep -E '^(Test|Fuzz)' test-names.txt | grep -Ev '^(Test|Fuzz)(Unit|Integration|API).+'; then \
	  echo 'Name every test TestUnit..., TestIntegration... or TestAPI... and every fuzz target FuzzUnit..., FuzzIntegration... or FuzzAPI...' >&2; exit 1; fi
	@if grep -E '^Example' test-names.txt; then \
	  echo 'Examples run outside the three suites; a service has no public Go API: write a TestUnit/TestIntegration/TestAPI instead' >&2; exit 1; fi

# All suites instrument the same whole module. No production files are excluded.
# Ordinary packages without tests may be reported as skipped; skipped Test events fail.
collect-unit collect-integration collect-api: test-names
	@set -euo pipefail; level='$(@:collect-%=%)'; case "$$level" in unit) prefix=Unit;; integration) prefix=Integration;; api) prefix=API;; esac; \
	rm -f "$$level.out" "$$level.jsonl"; \
	CI=true go test -json -race -count=1 -timeout=8m -coverpkg=./... \
	  -coverprofile="$$level.out" -run "^(Test|Fuzz)$$prefix" ./... | tee "$$level.jsonl"; \
	bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"

check-unit check-integration check-api:
	@set -euo pipefail; level='$(@:check-%=%)'; \
	bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"; \
	$(COVERAGE) --config .testcoverage.yml --profile "$$level.out" \
	  --breakdown-file-name "$$level.coverage.txt" | tee "$$level.report.txt"

cover-unit: collect-unit
	$(MAKE) check-unit
cover-integration: collect-integration
	$(MAKE) check-integration
cover-api: collect-api
	$(MAKE) check-api

cover-combined: ## informational union; the tool merges profiles, never percentages
	@set -euo pipefail; for level in unit integration api; do bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"; done
	set -euo pipefail; $(COVERAGE) --profile unit.out,integration.out,api.out \
	  --breakdown-file-name combined.coverage.txt | tee combined.report.txt

cover: collect-unit collect-integration collect-api ## collect all profiles before enforcing the three gates
	@status=0; for level in unit integration api; do $(MAKE) check-$$level || status=1; done; \
	$(MAKE) cover-combined || status=1; exit "$$status"

lint: ## shared pinned golangci-lint and rendered configuration
	@common=$$(python3 scripts/common_ci.py) && python3 -B "$$common/lint.py" lint --root "$(CURDIR)"

fmt: ## format using the same pinned linter and configuration
	@common=$$(python3 scripts/common_ci.py) && python3 -B "$$common/lint.py" fmt --root "$(CURDIR)"

image: ## the service image
	docker build -t bank-statement-data:dev .

up: ## PostgreSQL, migrations and the service in Docker
	docker compose up --build

down:
	docker compose down

run: build ## the service on the host against the compose PostgreSQL
	SERVICE_NAME=bank-statement-data ENV=dev AUTH_MODE=insecure-dev LOG_FORMAT=text \
	TBANK_LOGIN_URL="$${TBANK_LOGIN_URL:-https://www.tbank.ru/login/}" \
	DB_PRIMARY_DSN='postgres://bank_statement_data:bank_statement_data@localhost:5432/bank_statement_data?sslmode=disable' \
	$(BIN) serve

migrate: build ## migrate up against the compose PostgreSQL
	DB_PRIMARY_DSN='postgres://bank_statement_data:bank_statement_data@localhost:5432/bank_statement_data?sslmode=disable' $(BIN) migrate up

# FIN-372: shared lint source, identical locally and in CI.
BASE_REF ?= origin/main
.PHONY: lint-config lint-migrations
lint-config: ## render the declared shared linter profile without running Go
	python3 scripts/common_ci.py render

lint-migrations: ## immutable SQL and Squawk on added up migrations
	@common=$$(python3 scripts/common_ci.py) && python3 -B "$$common/lint.py" migrations --root "$(CURDIR)" --base "$(BASE_REF)"
