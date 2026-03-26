.PHONY: lint lint-check fmt test e2e e2e-local

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
