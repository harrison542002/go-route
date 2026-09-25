.PHONY: build build-proxy build-admin dev-up dev-down generate sqlc openapi migrate migrate-diff migrate-hash migrate-lint unit-test integration-test test-all

build: build-proxy build-admin

dev-up:
	docker compose up -d --wait

dev-down:
	docker compose down -v

build-proxy:
	go build -o bin/ ./cmd/go-route

build-admin:
	go build -o bin/ ./cmd/go-route-admin

generate: sqlc openapi
	go generate ./...

sqlc:
	go tool sqlc generate

openapi:
	go generate ./schemas/admin/gen

migrate:
	atlas migrate apply --env local

migrate-diff:
	atlas migrate diff $(NAME) --env local

migrate-hash:
	atlas migrate hash --env local

migrate-lint:
	atlas migrate lint --env local --latest 1

unit-test:
	go test ./...

integration-test:
	go test -tags integration ./tests/... -v

test-all: unit-test integration-test
