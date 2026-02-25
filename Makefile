.PHONY: dev build clean frontend backend docker docker-agent docker-all

# Development: run frontend dev server + Go backend
dev:
	@echo "Start backend: go run . serve --password test"
	@echo "Start frontend: cd web && pnpm dev"

# Build frontend then Go binary
build: frontend backend

frontend:
	cd web && pnpm install && pnpm build

backend:
	go build -o bin/cli-server .

clean:
	rm -rf bin/ web/dist/

docker:
	docker build -t cli-server .

docker-agent:
	docker build -f Dockerfile.agent -t cli-server-agent:latest .

docker-all: docker docker-agent
