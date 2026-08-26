.PHONY: build run test race e2e
build:
	go build -o bin/orc ./cmd/orc
run: build
	./bin/orc serve --addr 127.0.0.1:8080 --data ./.orchestrator
test:
	go vet ./... && go test ./...
race:
	go test -race ./...
e2e: build
	go test ./internal/api/ -run 'TestBrowser|TestExampleCase' -v
