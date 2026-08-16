# Sluice — common workflows.
#
# Everything here is also what CI runs, so "it passes locally" and "it passes
# in the pipeline" mean the same thing.

SHELL := /bin/sh
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/saumyapatel/sluice
GOFLAGS ?= -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

BIN := bin
CMDS := sluiced sluicectl sluice-upstream

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nSluice $(VERSION)\n\nusage: make <target>\n\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@echo

##@ Development

.PHONY: run
run: ## Run the self-contained demo on http://localhost:8080
	go run ./cmd/sluiced --demo

.PHONY: build
build: ## Build every binary into ./bin
	@mkdir -p $(BIN)
	@for cmd in $(CMDS); do \
		echo "building $$cmd"; \
		CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/$$cmd ./cmd/$$cmd || exit 1; \
	done

.PHONY: fmt
fmt: ## Format all Go source
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: test
test: ## Run the test suite
	go test ./...

.PHONY: race
race: ## Run the suite under the race detector (needs a 64-bit C toolchain)
	CGO_ENABLED=1 go test -race -count=2 ./...

.PHONY: cover
cover: ## Produce and summarise a coverage profile
	go test -coverprofile=coverage.out -covermode=atomic ./...
	@go tool cover -func=coverage.out | tail -n 1
	@echo "html report: go tool cover -html=coverage.out"

.PHONY: check
check: fmt vet test ## Format, vet and test — run this before pushing
	@git diff --quiet || { echo "gofmt changed files; commit them"; git diff --stat; exit 1; }
	@echo "all checks passed"

##@ Security

.PHONY: vuln
vuln: ## Scan dependencies and stdlib for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: sbom
sbom: ## Generate a CycloneDX SBOM
	go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@latest mod \
		-json -output sbom.json .
	@echo "wrote sbom.json"

.PHONY: token
token: ## Generate an API token suitable for SLUICE_API_TOKEN
	@openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'

.PHONY: certs
certs: ## Generate a local CA and SPIFFE workload certificates for the mTLS demo
	./scripts/gen-certs.sh

##@ Container

.PHONY: image
image: ## Build the container image
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):latest --build-arg VERSION=$(VERSION) .

.PHONY: image-smoke
image-smoke: image ## Build the image and verify it serves
	@docker rm -f sluice-smoke >/dev/null 2>&1 || true
	docker run -d --name sluice-smoke -p 18080:8080 $(IMAGE):$(VERSION) >/dev/null
	@echo "waiting for readiness..."
	@for i in $$(seq 1 40); do \
		curl -sf http://localhost:18080/readyz >/dev/null && break || sleep 1; \
	done
	@curl -sf http://localhost:18080/api/status >/dev/null && echo "api ok"
	@curl -sf http://localhost:18080/metrics | grep -q '^sluice_decisions_total' && echo "metrics ok"
	@docker rm -f sluice-smoke >/dev/null
	@echo "image smoke test passed"

.PHONY: compose-up
compose-up: ## Bring up the full stack: Envoy, three regions, control plane, Prometheus, Grafana
	docker compose -f deploy/docker-compose.yml up --build -d
	@echo
	@echo "  dashboard    http://localhost:8080    Sluice's own mission-control UI"
	@echo "  grafana      http://localhost:3000    fleet metrics over time"
	@echo "  prometheus   http://localhost:9090"
	@echo "  envoy admin  http://localhost:9901"
	@echo "  proxy        https://localhost:10000  mutual TLS; see deploy/README for a curl"
	@echo

.PHONY: dashboard-lint
dashboard-lint: ## Check the Grafana dashboard parses and every panel queries a real metric
	@python3 scripts/check-dashboard.py

.PHONY: compose-down
compose-down: ## Tear the stack down
	docker compose -f deploy/docker-compose.yml down -v

.PHONY: compose-logs
compose-logs: ## Follow the stack's logs
	docker compose -f deploy/docker-compose.yml logs -f

##@ Deployment

.PHONY: helm-lint
helm-lint: ## Lint and render the Helm chart
	helm lint deploy/helm/sluice
	helm template sluice deploy/helm/sluice --set api.token=lint-only >/dev/null
	helm template sluice deploy/helm/sluice --set api.token=lint-only --set serviceMonitor.enabled=true >/dev/null
	@echo "chart renders"

.PHONY: k8s-validate
k8s-validate: ## Dry-run the raw manifests against a cluster
	kubectl apply --dry-run=server -f deploy/k8s/sluice.yaml

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) coverage.out coverage.html sbom.json certs
