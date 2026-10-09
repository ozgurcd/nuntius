.PHONY: verify format-check build vet staticcheck test race govulncheck install
.NOTPARALLEL: verify

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
	go build -trimpath -o "$$(go env GOPATH)/bin/nuntius" ./cmd/nuntius
