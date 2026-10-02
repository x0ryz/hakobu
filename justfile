# List recipes
default:
    @just --list

# Build the hakobu binary
build:
    go build -o hakobu .

# Unit tests
test *args:
    go test ./... {{args}}

# Tests against the local Docker (deploys, backups)
test-docker *args:
    HAKOBU_DOCKER_TEST=1 go test ./... {{args}}

# Tests of the watchdog Worker (needs bun)
test-js:
    bun test internal/ops

# Regenerate the store code from migrations and queries
gen:
    sqlc generate

# Lint; pass --new-from-rev=main to see only new issues
lint *args:
    golangci-lint run ./... {{args}}

# Known vulnerabilities in reachable code
vuln:
    govulncheck ./...

# Everything CI runs
check: build test vuln
    golangci-lint run ./... --new-from-rev=main
    sqlc diff

# Validate the GoReleaser config and build a local snapshot into dist/
release-check:
    goreleaser check
    goreleaser release --snapshot --clean --skip=sign
