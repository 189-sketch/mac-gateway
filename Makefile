.PHONY: build dist test test-short clean

# Binaries for this machine.
build:
	go build -o bin/ ./cmd/...

# Release binaries: the gateway for the Linux server, the worker for both Mac types.
dist:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/fleet-gateway-linux-amd64 ./cmd/fleet-gateway
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist/fleet-worker-darwin-arm64 ./cmd/fleet-worker
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o dist/fleet-worker-darwin-amd64 ./cmd/fleet-worker

# The end-to-end tests depend on timing, so don't run them alongside other heavy work.
test:
	go test ./... -count=1 -timeout 10m

# Skips the test that holds a connection open for over 30 seconds.
test-short:
	go test ./... -count=1 -short -timeout 10m

clean:
	rm -rf bin dist
