.PHONY: build web test dev-api dev-web

build: web
	go build -o ctm ./cmd/ctm

web:
	cd web && pnpm install && pnpm build

test:
	go test ./...

# Development: API on :8088 and Vite (with hot reload) on :5173.
dev-api:
	go run ./cmd/ctm

dev-web:
	cd web && pnpm dev
