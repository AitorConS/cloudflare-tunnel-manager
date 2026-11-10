# --- web UI ---
FROM node:24-alpine AS web
WORKDIR /src/web
RUN npm install -g pnpm@11
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# --- server ---
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
COPY --from=web /src/internal/webui/dist internal/webui/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /ctm ./cmd/ctm

# --- cloudflared connector (pin a version; bump to update) ---
FROM cloudflare/cloudflared:2026.10.7 AS cloudflared

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=cloudflared /usr/local/bin/cloudflared /usr/local/bin/cloudflared
COPY --from=build /ctm /usr/local/bin/ctm
ENV CTM_DATA_DIR=/data
VOLUME /data
EXPOSE 8088
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -q -O /dev/null http://127.0.0.1:8088/api/status || exit 1
ENTRYPOINT ["ctm"]
