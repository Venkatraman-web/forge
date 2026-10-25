# Forge server image. Build: docker build -t forge .
# Two stages: compile with the Go toolchain, then copy only the static binary
# into a small image that runs as an unprivileged user.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY sdk ./sdk
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/forge ./cmd/forge

FROM alpine:3.20
RUN adduser -D -H -u 10001 forge
USER forge
COPY --from=build /out/forge /usr/local/bin/forge
# Forge reads all settings from FORGE_* environment variables (see compose.yaml).
EXPOSE 8080 8081 8082
ENTRYPOINT ["/usr/local/bin/forge"]
