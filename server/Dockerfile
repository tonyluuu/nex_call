FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w" -o /out/call-server ./cmd/call-server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app && mkdir /data && chown app /data
USER app
COPY --from=build /out/call-server /usr/local/bin/call-server
EXPOSE 8080
ENTRYPOINT ["call-server"]
CMD ["serve"]
