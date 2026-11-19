# 构建 linux/arm64 的 device-agent（仅 Debian bookworm，glibc 2.36）。
#   docker buildx build --platform linux/arm64 -o type=local,dest=./dist .
# 产物不能在 Ubuntu 18.04 上运行。Actions 与 18.04 香橙派请用 Dockerfile.bionic。
FROM golang:1.26-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends libopus-dev pkg-config && rm -rf /var/lib/apt/lists/*
WORKDIR /src
ENV CGO_ENABLED=1 GOFLAGS=-tags=nolibopusfile
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go vet ./... && go test ./... && \
    go build -trimpath -ldflags "-s -w" -o /out/device-agent ./cmd/device-agent

FROM scratch
COPY --from=build /out/device-agent /device-agent
