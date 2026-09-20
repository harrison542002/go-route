.PHONY: generate sqlc migrate migrate-diff migrate-hash migrate-lint unit-test integration-test test-all

generate: sqlc
	go generate ./...

sqlc:
	go tool sqlc generate

# Applies pending migrations to $DATABASE_URL.
migrate:
	atlas migrate apply --env local

# Writes a new migration from the difference between db/migrations and
# NAME's desired state. Usage: make migrate-diff NAME=add_something
migrate-diff:
	atlas migrate diff $(NAME) --env local

# Re-hashes atlas.sum. Required after editing any migration by hand.
migrate-hash:
	atlas migrate hash --env local

migrate-lint:
	atlas migrate lint --env local --latest 1

unit-test:
	go test ./...

integration-test:
	go test -tags integration ./tests/... -v

test-all: unit-test integration-test
