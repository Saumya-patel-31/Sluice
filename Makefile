# Sluice — common workflows.
#
# Everything here is also what CI runs, so "it passes locally" and "it passes
# in the pipeline" mean the same thing.

SHELL := /bin/sh
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/saumya-patel-31/sluice
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

.PHONY: bench
bench: ## Benchmark the decision path and the control loop
	go test ./internal/router -bench . -benchmem -run '^$$' | grep -v '^2[0-9]'

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

##@ Kubernetes

KIND_CLUSTER ?= sluice
K8S_IMAGE    ?= ghcr.io/saumya-patel-31/sluice:latest

.PHONY: kind-up
kind-up: ## Create a kind cluster and deploy the whole stack into it
	@kind get clusters 2>/dev/null | grep -qx "$(KIND_CLUSTER)" || \
		kind create cluster --name "$(KIND_CLUSTER)" --wait 120s
	docker build -t "$(K8S_IMAGE)" --build-arg VERSION=kind .
	kind load docker-image "$(K8S_IMAGE)" --name "$(KIND_CLUSTER)"
	@$(MAKE) k8s-deploy

.PHONY: k8s-deploy
k8s-deploy: k8s-certs ## Deploy to the current kubectl context
	kubectl apply -f deploy/k8s/10-regions.yaml
	kubectl apply -f deploy/k8s/15-envoy.yaml
	kubectl apply -f deploy/k8s/20-control-plane.yaml
	kubectl -n sluice rollout status deploy/sluice --timeout=180s
	kubectl -n sluice rollout status deploy/envoy  --timeout=180s
	@echo
	@kubectl -n sluice get pods
	@echo
	@echo "  verify:     make k8s-verify"
	@echo "  dashboard:  kubectl -n sluice port-forward svc/sluice 8080:8080"
	@echo "  token:      kubectl -n sluice get secret sluice-secrets -o jsonpath='{.data.api-token}' | base64 -d"

# The namespace and every Secret have to exist before the Deployments that
# mount them, or a first deploy on a fresh cluster sits in
# CreateContainerConfigError until somebody goes looking. Nothing here is
# committed: a private key or an API token in a manifest in git is a private
# key in everyone's clone.
#
# TRUST_DOMAIN is cluster.local because that is what the shipped policy
# recognises and what SPIFFE conventionally uses in Kubernetes. Issuing under
# any other domain deploys cleanly, reports every pod Ready, and then denies
# 100% of legitimate traffic — which is fail-closed and correct, and still took
# a packet capture to spot.
.PHONY: k8s-certs
k8s-certs: ## Create the namespace, API token and mTLS Secrets (idempotent)
	@kubectl get namespace sluice >/dev/null 2>&1 || kubectl create namespace sluice
	@kubectl -n sluice get secret sluice-secrets >/dev/null 2>&1 || \
		kubectl -n sluice create secret generic sluice-secrets \
			--from-literal=api-token="$$(openssl rand -hex 32)"
	@kubectl -n sluice get secret sluice-certs >/dev/null 2>&1 || { \
		TRUST_DOMAIN=cluster.local ./scripts/gen-certs.sh certs-k8s >/dev/null; \
		kubectl -n sluice create secret generic sluice-certs \
			--from-file=ca.crt=certs-k8s/ca.crt \
			--from-file=server.crt=certs-k8s/server.crt \
			--from-file=server.key=certs-k8s/server.key; \
		kubectl -n sluice create secret generic sluice-client-certs \
			--from-file=ca.crt=certs-k8s/ca.crt \
			--from-file=feed.crt=certs-k8s/feed.crt \
			--from-file=feed.key=certs-k8s/feed.key \
			--from-file=intruder.crt=certs-k8s/intruder.crt \
			--from-file=intruder.key=certs-k8s/intruder.key; \
	}

.PHONY: k8s-verify
k8s-verify: ## Drive real mTLS traffic through the deployed stack and assert it routed
	@./scripts/verify-k8s.sh

.PHONY: envoy-render
envoy-render: ## Regenerate the Kubernetes Envoy manifest from the canonical bootstrap
	@./scripts/render-k8s-envoy.sh

# One bootstrap, two deployments. A fix applied to the Compose config and not
# the Kubernetes one would be invisible until the cluster misbehaved.
.PHONY: envoy-render-check
envoy-render-check: ## Fail if deploy/k8s/15-envoy.yaml has drifted from the bootstrap
	@./scripts/render-k8s-envoy.sh /tmp/15-envoy.check.yaml >/dev/null
	@diff -u deploy/k8s/15-envoy.yaml /tmp/15-envoy.check.yaml >/dev/null || { \
		echo "deploy/k8s/15-envoy.yaml is stale; run: make envoy-render"; \
		diff -u deploy/k8s/15-envoy.yaml /tmp/15-envoy.check.yaml | head -30; \
		rm -f /tmp/15-envoy.check.yaml; exit 1; }
	@rm -f /tmp/15-envoy.check.yaml
	@echo "the Kubernetes Envoy manifest matches its source bootstrap"

.PHONY: k8s-validate
k8s-validate: ## Dry-run the manifests against the cluster's API server
	kubectl apply --dry-run=server -f deploy/k8s/10-regions.yaml
	kubectl apply --dry-run=server -f deploy/k8s/15-envoy.yaml
	kubectl apply --dry-run=server -f deploy/k8s/20-control-plane.yaml

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name "$(KIND_CLUSTER)"


##@ Deployment

.PHONY: helm-lint
helm-lint: ## Lint and render the Helm chart
	helm lint deploy/helm/sluice
	helm template sluice deploy/helm/sluice --set api.token=lint-only >/dev/null
	helm template sluice deploy/helm/sluice --set api.token=lint-only --set serviceMonitor.enabled=true >/dev/null
	@echo "chart renders"

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) coverage.out coverage.html sbom.json certs
