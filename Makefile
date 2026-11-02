.PHONY: build dist test test-short clean

# Binaries for this machine.
build:
	go build -o bin/ ./cmd/...

# Release binaries: the gateway for the Linux server, the worker for both Mac
# types, and the agent CLI for all three.
dist:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/fleet-gateway-linux-amd64 ./cmd/fleet-gateway
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist/fleet-worker-darwin-arm64 ./cmd/fleet-worker
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o dist/fleet-worker-darwin-amd64 ./cmd/fleet-worker
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -o dist/fleet-worker-windows-amd64.exe ./cmd/fleet-worker
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -trimpath -o dist/fleet-worker-windows-arm64.exe ./cmd/fleet-worker
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/fleet-linux-amd64 ./cmd/fleet
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o dist/fleet-darwin-arm64 ./cmd/fleet
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o dist/fleet-darwin-amd64 ./cmd/fleet

# The end-to-end tests depend on timing, so don't run them alongside other heavy work.
test:
	go test ./... -count=1 -timeout 10m

# Skips the test that holds a connection open for over 30 seconds.
test-short:
	go test ./... -count=1 -short -timeout 10m

clean:
	rm -rf bin dist
