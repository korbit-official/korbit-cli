# Copyright (c) 2026 Digital X Co., Ltd.
#
# SPDX-License-Identifier: Apache-2.0

BINARY := dgx-cli

.PHONY: build test vet fmt check licenses licenses-check dist dist-unsigned release release-unsigned notarize publish clean

build: ## Build the binary for the host platform
	go build -o $(BINARY) .

licenses: ## Regenerate THIRD_PARTY_LICENSES.txt (commit the result)
	go run ./tools/licensegen

licenses-check: ## Fail if THIRD_PARTY_LICENSES.txt is stale (for CI/release)
	./scripts/check-licenses.sh

test: ## Run the test suite
	go test ./...

vet: ## Static analysis
	go vet ./...

fmt: ## Check formatting (fails if anything is unformatted)
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi

check: fmt vet test ## Run all checks

dist: ## Build a local snapshot of all release artifacts (no publish, no notarize)
	goreleaser release --snapshot --clean

dist-unsigned: ## Snapshot build with signing forced off (fast; test the build without a cert)
	KORBIT_SKIP_MACOS_SIGN=1 KORBIT_SKIP_RSA_SIGN=1 goreleaser release --snapshot --clean

# The fill step turns install/install.{sh,ps1} into runnable, version-pinned
# installers in dist/ (embedding this release's archive checksums), which
# publish.sh then uploads. Real releases are built at an exact tag, so the tag is
# available here; snapshot builds (dist/) skip it — their URLs wouldn't resolve.
release: ## Build & sign the real release into dist/ — does NOT publish (no GitHub access)
	goreleaser release --clean
	./scripts/fill-install-templates.sh "$$(git describe --tags --exact-match)"

release-unsigned: ## Real release build with signing disabled (test without a cert)
	KORBIT_SKIP_MACOS_SIGN=1 KORBIT_SKIP_RSA_SIGN=1 goreleaser release --clean
	./scripts/fill-install-templates.sh "$$(git describe --tags --exact-match)"

notarize: ## Notarize the signed macOS binaries in dist/ (real releases only)
	./scripts/macos-notarize.sh dist

publish: ## Upload built dist/ artifacts to the GitHub Release (gh; KORBIT_RELEASE_REPO to override)
	./scripts/publish.sh dist

clean: ## Remove build artifacts
	rm -rf $(BINARY) dist
