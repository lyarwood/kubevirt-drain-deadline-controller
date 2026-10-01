SHELL=/bin/bash

IMG_REGISTRY ?= localhost:$(shell hack/kubevirtci.sh registry 2>/dev/null | cut -d: -f2)
IMG ?= $(IMG_REGISTRY)/deadline-eviction-controller:latest

FUNCTEST_EXTRA_ARGS ?=

.PHONY: all
all: build test test-integration

.PHONY: build
build:
	go build ./...

.PHONY: test
test:
	go test ./pkg/... -v

.PHONY: test-integration
test-integration:
	cd tests/integration && KUBEBUILDER_ASSETS="$(shell setup-envtest use 1.26.0 --bin-dir ../../bin -p path)" \
	  go test ./... -v

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: cluster-up
cluster-up:
	hack/kubevirtci.sh up

.PHONY: cluster-down
cluster-down:
	hack/kubevirtci.sh down

.PHONY: cluster-sync
cluster-sync: docker-build
	docker push $(IMG)
	KUBECONFIG=$$(hack/kubevirtci.sh kubeconfig) kubectl apply -k config/default
	KUBECONFIG=$$(hack/kubevirtci.sh kubeconfig) hack/wait.sh

.PHONY: cluster-functest
cluster-functest:
	cd tests/functional && KUBECONFIG=$$(../../hack/kubevirtci.sh kubeconfig) go test -v -timeout 0 ./... -ginkgo.v -ginkgo.randomize-all $(FUNCTEST_EXTRA_ARGS)

.PHONY: functest
functest:
	cd tests/functional && KUBECONFIG=$(KUBECONFIG) go test -v -timeout 0 ./... -ginkgo.v -ginkgo.randomize-all $(FUNCTEST_EXTRA_ARGS)
