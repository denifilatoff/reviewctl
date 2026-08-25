.PHONY: build ci fmt test vet apm-check live-e2e launchd-smoke

build:
	@build_dir="$$(mktemp -d)"; trap 'rm -rf "$$build_dir"' EXIT; go build -o "$$build_dir/reviewctl" .

fmt:
	@test -z "$$(gofmt -l *.go)"

test:
	go test ./...
	sh scripts/install_test.sh

vet:
	go vet ./...

apm-check:
	@install_dir="$$(mktemp -d)"; trap 'rm -rf "$$install_dir"' EXIT; \
		apm install --frozen --root "$$install_dir" --target codex

ci: fmt vet test build apm-check

live-e2e: build
	@build_dir="$$(mktemp -d)"; trap 'rm -rf "$$build_dir"' EXIT; \
		go build -o "$$build_dir/reviewctl" .; \
		REVIEWCTL_BIN="$$build_dir/reviewctl" ./scripts/live-e2e.sh

launchd-smoke:
	@build_dir="$$(mktemp -d)"; trap 'rm -rf "$$build_dir"' EXIT; \
		go build -o "$$build_dir/reviewctl" .; \
		REVIEWCTL_BIN="$$build_dir/reviewctl" ./scripts/launchd-smoke.sh
