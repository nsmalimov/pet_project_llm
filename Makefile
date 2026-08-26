.PHONY: build run test race e2e verify docker fmt-check
GO ?= go
build:
	$(GO) build -o bin/orc ./cmd/orc
run: build
	./bin/orc serve --addr 127.0.0.1:8080 --data ./.orchestrator
test:
	$(GO) vet ./... && $(GO) test ./...
race:
	$(GO) test -race ./...
e2e: build
	$(GO) test ./internal/api/ -run 'TestBrowser|TestExampleCase|TestCancelOverHTTP' -v
# verify = the full contract: build, vet, unit/integration tests, race tests,
# product acceptance (HTTP + browser when Chrome is present).
fmt-check:
	@bad="$$(gofmt -l . | grep -v '^old/')"; if [ -n "$$bad" ]; then echo "gofmt needed:"; echo "$$bad"; exit 1; fi
verify: build fmt-check
	$(GO) vet ./...
	$(GO) test ./...
	$(GO) test -race ./internal/store/ ./internal/proof/ ./internal/sandbox/ ./internal/engine/ -run 'TestSaveTaskIs|TestConcurrent|TestTwoWorkers|TestIdempotent|TestResolveDecisionTwice|TestFlaky|TestCommandInjection|TestBackground'
	$(GO) test ./internal/api/ -run 'TestBrowser|TestExampleCase|TestCancelOverHTTP|TestCrossTenant|TestRoleMatrix'
	@echo "verify: OK"
docker:
	docker build -t proofline:dev .
