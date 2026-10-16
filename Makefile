.PHONY: verify format-check build vet staticcheck test race govulncheck install release-check changelog-check release-notes-check release-snapshot
.NOTPARALLEL: verify

GORELEASER_VERSION := v2.17.0
RELEASE_NOTES_OUTPUT ?= /dev/null
RELEASE_TEST_TMPDIR ?= /tmp
RELEASE_VERSION ?= $(shell go run ./cmd/nuntius version | cut -d ' ' -f 2)
RELEASE_NOTES_FILE ?= RELEASE_NOTES.md
CHANGELOG_FILE ?= CHANGELOG.md

verify: format-check build vet staticcheck test race govulncheck menubar-verify indicator-test redaction-proof

# Offline, immutable published sources; the tests never fetch or use real agents.
.PHONY: verify-compat
verify-compat:
	go test -tags=integration . -run '^TestP0(MixedVersions|LostClientReconciliation)$$' -count=1 -v

.PHONY: redaction-proof
# Deterministic recipes execute independently of coverage-guided seed selection.
# Fuzz payloads are recipe bytes; disposable valid keys exist only in test memory.
redaction-proof:
	go test . -run '^(TestIndependentReview|TestRedaction|TestReview|TestRereview|TestRound3|TestHotfixReview|Fuzz)' -count=1 -v
	go test . -run '^$$' -fuzz '^FuzzRedactionBytes$$' -fuzztime=60s -parallel=2
	go test . -run '^$$' -fuzz '^FuzzRedactionEventTopology$$' -fuzztime=60s -parallel=2
	go test . -run '^$$' -fuzz '^FuzzRedactionPrefixes$$' -fuzztime=60s -parallel=2
	go test . -run '^$$' -fuzz '^FuzzRedactionCacheSchedule$$' -fuzztime=60s -parallel=2
	go test . -run '^$$' -fuzz '^FuzzRedactionPublicSinks$$' -fuzztime=60s -parallel=2

.PHONY: menubar-verify indicator-test
menubar-verify:
	cd menubar && go build -o ../bin/nuntius-menubar .
	cd menubar && go vet ./...
	cd menubar && staticcheck ./...
	cd menubar && go test ./... -count=1
	cd menubar && go test -race ./... -count=1
	cd menubar && govulncheck ./...

indicator-test:
	node testdata/indicator-test.js

HELPER_OS ?= $(shell go env GOOS)
HELPER_ARCH ?= $(shell go env GOARCH)
HELPER_OUTPUT := $(CURDIR)/release-helpers/$(HELPER_OS)_$(HELPER_ARCH)
.PHONY: menubar-build
menubar-build:
	mkdir -p "$(HELPER_OUTPUT)"
	cd menubar && CGO_ENABLED=$(if $(filter darwin,$(HELPER_OS)),1,0) GOOS=$(HELPER_OS) GOARCH=$(HELPER_ARCH) go build -trimpath -ldflags='-s -w' -o "$(HELPER_OUTPUT)/nuntius-menubar" .
	@set -eu; if [ "$(HELPER_OS)" = darwin ]; then \
		bundle="$(HELPER_OUTPUT)/Nuntius Menu Bar.app/Contents"; \
		mkdir -p "$$bundle/MacOS" "$$bundle/Resources/Relay.iconset"; \
		cp menubar/Info.plist "$$bundle/Info.plist"; \
		cp "$(HELPER_OUTPUT)/nuntius-menubar" "$$bundle/MacOS/nuntius-menubar"; \
		for size in 16 32 128 256 512; do \
			cp "menubar/assets/nuntius-app-icon-$$size.png" "$$bundle/Resources/Relay.iconset/icon_$${size}x$${size}.png"; \
			double=$$((size * 2)); cp "menubar/assets/nuntius-app-icon-$$double.png" "$$bundle/Resources/Relay.iconset/icon_$${size}x$${size}@2x.png"; \
		done; \
		python3 -c 'import pathlib,struct,sys; parts=[tag.encode()+struct.pack(">I",8+len(b))+b for tag,size in [("icp4",16),("icp5",32),("icp6",64),("ic07",128),("ic08",256),("ic09",512),("ic10",1024)] for b in [pathlib.Path("menubar/assets/nuntius-app-icon-"+str(size)+".png").read_bytes()]]; data=b"".join(parts); pathlib.Path(sys.argv[1]).write_bytes(b"icns"+struct.pack(">I",8+len(data))+data)' "$$bundle/Resources/Relay.icns"; \
		rm -rf "$$bundle/Resources/Relay.iconset"; \
		codesign --force --sign - "$(HELPER_OUTPUT)/nuntius-menubar"; \
		codesign --force --sign - "$(HELPER_OUTPUT)/Nuntius Menu Bar.app"; \
		codesign --verify --strict "$(HELPER_OUTPUT)/Nuntius Menu Bar.app"; \
	fi

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
	# Sequential root families retain the default timeout for every invocation.
	go test -race -count=1 -run '^TestNative.*$$' .
	go test -race -count=1 -run '^Test(Goal|DispatchGoalID|Brief|LinkedWait|VerifiedDependencies|CoveredClosure|Coverage|DependencyCycles).*$$' .
	go test -race -count=1 -run '^(Test(Redaction|IndependentReview|Review|Rereview|Round3|HotfixReview|Private|ReleasePrivate|ReleaseSplit|ReleasePublic)|Fuzz).*$$' .
	# Keep coverage/assertions and bound CPU parallelism across the field processes.
	GOMAXPROCS=2 go test -race -count=1 -run '^TestControlMonitorMCPDispatchSeries$$' .
	# The complement also runs new tests and fuzz seeds outside those families.
	go test -race -count=1 -run '.' -skip '^(TestControlMonitorMCPDispatchSeries|(Test(Native|Goal|DispatchGoalID|Brief|LinkedWait|VerifiedDependencies|CoveredClosure|Coverage|DependencyCycles|Redaction|IndependentReview|Review|Rereview|Round3|HotfixReview|Private|ReleasePrivate|ReleaseSplit|ReleasePublic)|Fuzz).*)$$' .
	# Keep every other package from ./... in the race gate, without root repeats.
	go test -race -count=1 $(filter-out $(shell go list .),$(shell go list ./...))

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
	$(MAKE) release-artifacts-check

.PHONY: release-artifacts-check
release-artifacts-check:
	@python3 -c 'import pathlib,tarfile; archives=list(pathlib.Path("dist").glob("nuntius_*.tar.gz")); assert len(archives)==4, "expected Darwin/Linux amd64/arm64 archives"; required=["nuntius","nuntius-menubar","menubar-licenses/systray.txt","menubar-licenses/dbus.txt","menubar-licenses/x-sys.txt"]; app="Nuntius Menu Bar.app/Contents/"; bundle=[app+"Info.plist",app+"MacOS/nuntius-menubar",app+"Resources/Relay.icns",app+"_CodeSignature/CodeResources"]; [(lambda t,p: ([(lambda m: (m.isfile() and m.size>0) or (_ for _ in ()).throw(AssertionError("missing payload")))(t.getmember(n)) for n in required+(bundle if "Darwin" in p.name else [])], [(_ for _ in ()).throw(AssertionError("helper not executable")) for n in ["nuntius","nuntius-menubar"]+( [app+"MacOS/nuntius-menubar"] if "Darwin" in p.name else []) if t.getmember(n).mode & 0o111 == 0]))(tarfile.open(p),p) for p in archives]; print("release archive payloads passed")'

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
