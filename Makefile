.PHONY: verify format-check build vet staticcheck test race govulncheck install release-check changelog-check release-notes-check release-snapshot
.NOTPARALLEL: verify

GORELEASER_VERSION := v2.17.0
RELEASE_NOTES_OUTPUT ?= /dev/null
RELEASE_TEST_TMPDIR ?= /tmp
RELEASE_VERSION ?= $(shell go run ./cmd/nuntius version | cut -d ' ' -f 2)
RELEASE_NOTES_FILE ?= RELEASE_NOTES.md
CHANGELOG_FILE ?= CHANGELOG.md

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

release-check: release-notes-check
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) check --config .goreleaser.yaml

changelog-check:
	@awk -v heading="## $(RELEASE_VERSION) — " 'index($$0, heading) == 1 { found = 1; next } found && /^## / { exit } found && /[^[:space:]]/ { content = 1 } END { if (!found || !content) exit 1 }' "$(CHANGELOG_FILE)" || { echo 'Release version needs a nonempty CHANGELOG section' >&2; exit 1; }

release-notes-check: changelog-check
	@awk -v heading="## $(RELEASE_VERSION) — " 'index($$0, heading) == 1 { found = 1; next } found && /^## / { exit } found { print; if ($$0 ~ /[^[:space:]]/) content = 1 } END { if (!found || !content) exit 1 }' "$(RELEASE_NOTES_FILE)" > "$(RELEASE_NOTES_OUTPUT)" || { echo 'Release version needs a nonempty RELEASE_NOTES section' >&2; exit 1; }

release-snapshot: release-check
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release --snapshot --clean --skip=publish

# Offline fixtures exercise the same selectors used by the release workflow.
.PHONY: release-notes-test
release-notes-test:
	@set -eu; fixture="$$(mktemp -d "$(RELEASE_TEST_TMPDIR)/nuntius-notes.XXXXXX")"; trap 'rm -rf "$$fixture"' EXIT; \
	printf '## v0.2.10 — fixture\n\nUser-facing change.\n\n## v0.2.1 — fixture\n\nOlder change.\n' > "$$fixture/notes.md"; \
	printf '## v0.2.10 — fixture\n\nTechnical detail.\n' > "$$fixture/changelog.md"; \
	$(MAKE) --no-print-directory release-notes-check RELEASE_VERSION=v0.2.10 RELEASE_NOTES_FILE="$$fixture/notes.md" CHANGELOG_FILE="$$fixture/changelog.md" RELEASE_NOTES_OUTPUT="$$fixture/selected.md"; \
	grep -Fq 'User-facing change.' "$$fixture/selected.md"; \
	if grep -Fq 'Older change.' "$$fixture/selected.md"; then echo 'selected another release' >&2; exit 1; fi; \
	for scenario in missing absent prefix empty changelog; do \
		case "$$scenario" in \
			missing) rm "$$fixture/notes.md" ;; \
			absent) printf '## v0.2.9 — fixture\n\nOther release.\n' > "$$fixture/notes.md" ;; \
			prefix) printf '## v0.2.100 — fixture\n\nOther release.\n' > "$$fixture/notes.md" ;; \
			empty) printf '## v0.2.10 — fixture\n\n## v0.2.9 — fixture\n\nOther release.\n' > "$$fixture/notes.md" ;; \
			changelog) printf '## v0.2.10 — fixture\n\nUser-facing change.\n' > "$$fixture/notes.md"; printf '## v0.2.9 — fixture\n\nOther release.\n' > "$$fixture/changelog.md" ;; \
		esac; \
		if $(MAKE) --no-print-directory release-notes-check RELEASE_VERSION=v0.2.10 RELEASE_NOTES_FILE="$$fixture/notes.md" CHANGELOG_FILE="$$fixture/changelog.md" RELEASE_NOTES_OUTPUT="$$fixture/selected.md" > "$$fixture/output" 2>&1; then echo "release notes accepted $$scenario fixture" >&2; exit 1; fi; \
	done; \
	echo 'release notes fixtures passed'
