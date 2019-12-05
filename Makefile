SHELL := /bin/bash
GITCOMMIT=$(shell git rev-parse --short HEAD)$(shell [[ $$(git status --porcelain) = "" ]] || echo -dirty)
LDFLAGS="-X main.gitCommit=$(GITCOMMIT)"
#REGISTRY    ?= quay.io/openshift/
IMAGE        = $(REGISTRY)managed-cluster-config:$(GITCOMMIT)

.PHONY: run
run:
	operator-sdk up local

.PHONY: generate
generate:
	go generate ./...
	operator-sdk generate openapi
	operator-sdk generate k8s

.PHONY: test
test:
	go test ./...

.PHONY: verify
verify:
	go run hack/validate-imports/validate-imports.go cmd hack pkg
	hack/verify/validate-code-format.sh 
	hack/verify/validate-generated.sh

.PHONY: build
build:
	go build -ldflags ${LDFLAGS} -o _data/template ./cmd/template/ 

.PHONY: image
image:
	podman image build -t ${IMAGE} --file=Dockerfile

