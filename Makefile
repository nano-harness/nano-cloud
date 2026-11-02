.PHONY: lint lint-check fmt test e2e e2e-local build clean quickstart stop logs reset runtime-images

build:
	go build -o bin/gateway ./cmd/gateway
	go build -o bin/net-policy-proxy ./cmd/net-policy-proxy
	go build -o bin/runtime-cli ./cmd/runtime-cli
	go build -o bin/runtime-cli-wrapper ./cmd/runtime-cli-wrapper
	go build -o bin/runtime-nano-agent ./cmd/runtime-nano-agent
	go build -o bin/worker ./cmd/worker

clean:
	rm -rf bin/

quickstart:
	./scripts/connect.sh

stop:
	docker compose down

logs:
	docker compose logs -f

reset:
	docker compose down -v --remove-orphans
	rm -rf .workdir

runtime-images:
	BUILD_RUNTIME_IMAGES=1 ./scripts/connect.sh

lint:
	golangci-lint run --fix

lint-check:
	golangci-lint run
	revive -config revive.toml -formatter friendly ./...

fmt:
	goimports -w .
	go fmt ./...

test:
	go test ./...

# Fast E2E (in-memory gateway using httptest, no Docker required)
e2e:
	go test -v ./pkg/server -run 'TestRunEventsSSE|TestSSE_ReconnectAndHistory|TestWorker_WebSocketReconnect|TestRunRequest_SessionId|TestPairingApproveByShortCodeFlow|TestConsoleApproveByCodePrefill' -count=1

# Local E2E (requires Docker; builds images, starts gateway+worker binaries)
e2e-local:
	./scripts/local-e2e.sh
