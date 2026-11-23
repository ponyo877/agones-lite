.PHONY: build test lint

build:
	go build -o bin/agones-lite ./cmd/agones-lite

test:
	go test ./...

lint:
	golangci-lint run ./...

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
	docker build -t agones-lite-sidecar:dev -f build/sidecar/Dockerfile bin
