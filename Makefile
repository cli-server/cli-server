.PHONY: dev build clean frontend backend agent agent-all docker docker-agent docker-all

# Development: run frontend dev server + Go backend
dev:
	@echo "Start backend: go run . serve --password test"
	@echo "Start frontend: cd web && pnpm dev"

# Build frontend then Go binary
build: frontend backend

frontend:
	cd web && pnpm install && pnpm build

backend:
	go build -o bin/agentserver .

agent:
	CGO_ENABLED=0 go build -o bin/agentserver-agent ./cmd/agentserver-agent

agent-all:
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o bin/agentserver-linux-amd64        ./cmd/agentserver-agent
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -o bin/agentserver-linux-arm64        ./cmd/agentserver-agent
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -o bin/agentserver-darwin-amd64       ./cmd/agentserver-agent
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -o bin/agentserver-darwin-arm64       ./cmd/agentserver-agent
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o bin/agentserver-windows-amd64.exe  ./cmd/agentserver-agent

clean:
	rm -rf bin/ web/dist/

docker:
	docker build -t agentserver .

docker-agent:
	docker build -f Dockerfile.agent -t agentserver-agent:latest .

docker-all: docker docker-agent
