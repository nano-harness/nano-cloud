.PHONY: lint lint-check fmt test

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
