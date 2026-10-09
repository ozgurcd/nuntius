.PHONY: verify format-check build vet staticcheck test race govulncheck install release-check release-notes-check release-snapshot
.NOTPARALLEL: verify

GORELEASER_VERSION := v2.17.0
RELEASE_NOTES_OUTPUT ?= /dev/null

verify: format-check build vet staticcheck test race govulncheck

format-check:
	@test -z "$$(gofmt -l .)" || { echo 'Run gofmt on the listed files'; gofmt -l .; exit 1; }

build:
	go build ./...

vet:
	go vet ./...

staticcheck:
	staticcheck ./...

test:
	go test ./... -count=1

race:
	go test -race ./... -count=1

govulncheck:
	govulncheck ./...

install:
	brew install --cask ozgurcd/tap/nuntius

release-check:
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) check --config .goreleaser.yaml

release-notes-check:
	@version="$$(go run ./cmd/nuntius version | cut -d ' ' -f 2)"; \
	awk -v heading="## $$version — " 'index($$0, heading) == 1 { found = 1; next } found && /^## / { exit } found { print } END { if (!found) exit 1 }' CHANGELOG.md > "$(RELEASE_NOTES_OUTPUT)"

release-snapshot: release-check
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release --snapshot --clean --skip=publish
