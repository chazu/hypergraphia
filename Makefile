BINARY := hypergraphia
PKG := ./cmd/hypergraphia

# install location: GOBIN if set, else GOPATH/bin
GOBIN := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif
INSTALL_PATH := $(GOBIN)/$(BINARY)

# ad-hoc code signing (macOS only; no-op elsewhere)
UNAME := $(shell uname -s)
ifeq ($(UNAME),Darwin)
CODESIGN := codesign --force --sign -
else
CODESIGN := true
endif

SKILL_SRC := skills/hypergraphia
SKILL_DEST := $(HOME)/.claude/skills/hypergraphia

.PHONY: build install install-skill sign verify clean test vet bench e2e

## build: compile + ad-hoc sign the local binary
build:
	go build -o $(BINARY) $(PKG)
	$(CODESIGN) $(BINARY)

## install: go install + ad-hoc sign into $(GOBIN)
install:
	go install $(PKG)
	$(CODESIGN) $(INSTALL_PATH)
	@echo "installed + ad-hoc signed: $(INSTALL_PATH)"

## sign: re-sign the installed binary (ad-hoc)
sign:
	$(CODESIGN) $(INSTALL_PATH)

## verify: check the installed binary's signature
verify:
	codesign --verify --verbose $(INSTALL_PATH)

## install-skill: sync the agent skill into ~/.claude/skills (`hypergraphia init`
## does the same from the copy embedded in the binary)
install-skill:
	mkdir -p $(SKILL_DEST)
	cp -R $(SKILL_SRC)/ $(SKILL_DEST)/
	@echo "synced skill: $(SKILL_DEST)"

## test: run the test suite
test:
	go test ./...

## bench: recall and index benchmarks
bench:
	go test -run '^$$' -bench . -benchtime 20x ./internal/recall/

## e2e: sync, compaction and history push against a real GitHub repo
## (HYPERGRAPHIA_E2E_REPO=owner/name, default chazu/hypergraphia-notes-spike)
e2e:
	HYPERGRAPHIA_E2E_GITHUB=1 go test -run TestGitHubE2E -v -timeout 300s ./internal/app/

## vet: static checks
vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)

## clean: remove the local build artifact
clean:
	rm -f $(BINARY)
