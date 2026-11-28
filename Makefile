.PHONY: build test lint check dist models

VERSION ?= dev
LDFLAGS := -s -w -X github.com/ponyo877/scrim/pkg/version.Version=$(VERSION)

build:
	go build -o bin/scrim ./cmd/scrim

# Release binaries for every supported platform, into dist/.
# Usage: make dist VERSION=v1.0.0
#
# -trimpath leaves out where the source was, so that the same commit, built
# with the same Go, gives the same bytes anywhere: a release can be rebuilt
# and compared with checksums.txt.
dist:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/scrim-linux-amd64 ./cmd/scrim
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/scrim-linux-arm64 ./cmd/scrim
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/scrim-darwin-amd64 ./cmd/scrim
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/scrim-darwin-arm64 ./cmd/scrim
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/sidecar-linux-amd64 ./cmd/sidecar
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o dist/sidecar-linux-arm64 ./cmd/sidecar

test:
	go test ./...

lint:
	golangci-lint run ./...

# What CI checks, as far as one machine can: see .github/workflows/ci.yml,
# which also builds for every platform, tests on three kinds of machine and
# regenerates the protocol code. Run this before a pull request, and CI has
# less to say.
check: lint
	go mod tidy -diff
	go test -race -shuffle=on -count=1 ./...
	build/ci/properties.sh 1000
	build/ci/coverage.sh 79
	build/ci/fuzz.sh 10
	build/ci/pinned.sh
	go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
	cd formal && lake build --wfail
	build/ci/models.sh

# The Quint models in formal/quint: every state each of them can reach.
# After changing a table of a model, write its answers down again for the Go
# tests: build/ci/models.sh vectors
models:
	build/ci/models.sh

.PHONY: proto sidecar-image

# Regenerate the Go code for our own protocols. Needs protoc on the PATH.
proto:
	go build -o bin/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	go build -o bin/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc
	protoc --plugin=bin/protoc-gen-go --plugin=bin/protoc-gen-go-grpc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/sidecar/v1/sidecar.proto proto/node/v1/node.proto

# Build the sidecar image for the local Docker daemon's architecture.
sidecar-image:
	CGO_ENABLED=0 GOOS=linux go build -ldflags='-s -w' -o bin/sidecar-linux ./cmd/sidecar
	docker build -t scrim-sidecar:dev -f build/sidecar/Dockerfile bin
