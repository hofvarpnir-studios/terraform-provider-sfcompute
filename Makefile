SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
GO ?= go

# Code generators, pinned. `go run pkg@version` builds each tool from its own
# go.mod, so the tools' dependencies never mix with the provider's.
TFPLUGINGEN_OPENAPI   := github.com/hashicorp/terraform-plugin-codegen-openapi/cmd/tfplugingen-openapi@v0.3.0
TFPLUGINGEN_FRAMEWORK := github.com/hashicorp/terraform-plugin-codegen-framework/cmd/tfplugingen-framework@v0.4.1
TFPLUGINDOCS          := github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@v0.25.0

SFCGEN := $(GO) run ./tools/sfcgen

.PHONY: default build install test testacc vet fmt fetch-spec generate codegen docs clean

default: build

build:
	$(GO) build ./...

install:
	$(GO) install .

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -s -w $$(git ls-files '*.go')

# Refresh spec/ from docs.sfcompute.com (needs network).
fetch-spec:
	$(GO) run ./scripts/fetch-spec

# Regenerate everything derived from spec/openapi.json and generator_config.yml.
generate: codegen docs

codegen:
	rm -rf build internal/generated
	$(SFCGEN) prep -in spec/openapi.json -out build/openapi.json
	$(SFCGEN) resolve -spec build/openapi.json -config generator_config.yml \
		-out build/generator_config.yml -plan build/plan.json -coverage generator/coverage.txt
	$(GO) run $(TFPLUGINGEN_OPENAPI) generate --config build/generator_config.yml \
		--output build/provider_code_spec.raw.json build/openapi.json
	$(SFCGEN) patch -in build/provider_code_spec.raw.json -plan build/plan.json -out build/provider_code_spec.json
	$(GO) run $(TFPLUGINGEN_FRAMEWORK) generate resources --input build/provider_code_spec.json --output internal/generated
	$(GO) run $(TFPLUGINGEN_FRAMEWORK) generate data-sources --input build/provider_code_spec.json --output internal/generated
	$(SFCGEN) slim -dir internal/generated
	$(SFCGEN) registry -plan build/plan.json -out internal/provider/registry_gen.go
	gofmt -s -w internal/generated internal/provider/registry_gen.go
	$(GO) build ./...

# Provider documentation (docs/) from the provider schema, templates/ and
# examples/. tfplugindocs needs a terraform binary; it downloads one if none
# is on PATH.
docs:
	$(GO) run $(TFPLUGINDOCS) generate --provider-name sfcompute --rendered-provider-name "SF Compute"

clean:
	rm -rf build dist terraform-provider-sfcompute
