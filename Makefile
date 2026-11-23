.PHONY: build web test lint demo docker

build: web
	go build -o bin/agentws ./cmd/agentws

web:
	cd web && npm ci && npm run build

test:
	go test -race ./...
	cd web && npm run typecheck

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

# Try the dashboard without an API key.
demo: build
	./bin/agentws serve --demo

docker:
	docker build -t agentws .
