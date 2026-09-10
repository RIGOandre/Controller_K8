IMG ?= ghcr.io/rigoandre/controller_k8:dev
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5

.PHONY: help
help: ## Lista os alvos
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-16s %s\n", $$1, $$2}'

.PHONY: generate
generate: ## Regera deepcopy, CRD e RBAC a partir das marcações
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=preview-operator paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: verify-generate
verify-generate: generate ## Falha se o gerado estiver defasado do código
	@git diff --exit-code -- api config || \
		{ echo "arquivos gerados fora de dia: rode 'make generate' e commite"; exit 1; }

.PHONY: fmt vet test
fmt: ## gofmt
	gofmt -l -w .
vet: ## go vet
	go vet ./...
test: ## Testes com cobertura
	go test ./... -race -coverprofile=cover.out -covermode=atomic
	@go tool cover -func=cover.out | tail -1

.PHONY: build docker-build
build: ## Compila o manager
	go build -o bin/manager ./cmd/manager
docker-build: ## Constrói a imagem
	docker build -t $(IMG) .

.PHONY: install uninstall deploy undeploy
install: ## Aplica só o CRD
	kubectl apply -f config/crd/bases
uninstall: ## Remove o CRD
	kubectl delete -f config/crd/bases
deploy: install ## Aplica CRD, RBAC e o manager
	kubectl apply -f config/manager/namespace.yaml
	kubectl apply -f config/rbac
	kubectl apply -f config/manager
undeploy: ## Remove o manager
	kubectl delete -f config/manager --ignore-not-found
	kubectl delete -f config/rbac --ignore-not-found
