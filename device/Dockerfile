# 构建 linux/arm64 的 device-agent（香橙派 / Debian bookworm，glibc 2.36）。
#   docker buildx build --platform linux/arm64 -o type=local,dest=./dist .
# 产物 dist/device-agent 拷贝到香橙派即可运行，运行时依赖 libopus0。
# Ubuntu 18.04 请改用 Dockerfile.bionic，本文件的产物在 glibc 2.27 上无法启动。
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
