# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tinystack ./cmd/tinystack

FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 tinystack \
    && mkdir /data && chown tinystack /data

COPY --from=build /out/tinystack /usr/local/bin/tinystack
USER tinystack

EXPOSE 4566
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s \
    CMD wget -q -O /dev/null http://127.0.0.1:4566/_tinystack/health || exit 1

ENTRYPOINT ["tinystack"]
CMD ["start", "--host", "0.0.0.0", "--port", "4566"]
