# Stage 1: Build admin frontend
FROM node:25-slim AS frontend
RUN npm install -g pnpm
WORKDIR /app/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# Stage 2: Build opencode frontend from submodule
FROM oven/bun:1 AS opencode-frontend
WORKDIR /app
COPY opencode/ ./
RUN bun install --frozen-lockfile
RUN bun run --filter=@opencode-ai/app build

# Stage 3: Build Go backend
FROM golang:1.26-trixie AS backend
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/web/dist ./web/dist
COPY --from=opencode-frontend /app/packages/app/dist ./opencodeweb/dist
RUN CGO_ENABLED=0 go build -o cli-server .

# Stage 4: Runtime image with Docker CLI (claude-code runs in agent containers)
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl gnupg \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg \
    && chmod a+r /etc/apt/keyrings/docker.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian bookworm stable" \
       > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli \
    && rm -rf /var/lib/apt/lists/*
COPY --from=backend /app/cli-server /usr/local/bin/cli-server
EXPOSE 8080
ENTRYPOINT ["cli-server", "serve"]
