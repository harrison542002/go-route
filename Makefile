.PHONY: generate unit-test integration-test test-all
generate:
	go generate ./...

unit-test:
	go test ./...

integration-test:
	go test -tags integration ./tests/... -v

test-all: unit-test integration-test