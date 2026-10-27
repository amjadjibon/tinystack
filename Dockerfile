# syntax=docker/dockerfile:1

# docker build .               TinyStack with the web console on :4567
# docker build --target headless . TinyStack without the console (no Node.js stage)
# docker build --target console-cli . the console image plus the AWS CLI for CloudShell (about 350 MB in total)

FROM node:24-alpine AS console

WORKDIR /ui
RUN npm install -g pnpm@12
COPY ui/package.json ui/pnpm-lock.yaml ui/pnpm-workspace.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
COPY ui/ ./
RUN pnpm build

FROM golang:1.27-alpine AS source

WORKDIR /src
COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

FROM source AS build-headless

RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tinystack ./cmd/tinystack

FROM source AS build

COPY --from=console /ui/dist/console ui/dist/console
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -tags console -trimpath -ldflags="-s -w" -o /out/tinystack ./cmd/tinystack

FROM alpine:3.22 AS runtime

LABEL org.opencontainers.image.licenses="Apache-2.0" \
    org.opencontainers.image.source="https://github.com/amjadjibon/tinystack"
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 tinystack \
    && mkdir /data && chown tinystack /data

USER tinystack
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s \
    CMD wget -q -O /dev/null http://127.0.0.1:4566/_tinystack/health || exit 1

ENTRYPOINT ["tinystack"]
CMD ["start", "--host", "0.0.0.0", "--port", "4566"]

FROM runtime AS headless

COPY --from=build-headless /out/tinystack /usr/local/bin/tinystack
EXPOSE 4566

FROM runtime AS console-base

COPY --from=build /out/tinystack /usr/local/bin/tinystack
# 4566 is the AWS endpoint, 4567 the web console.
EXPOSE 4566 4567

# CloudShell runs the AWS CLI it finds on PATH; mandoc and the doc package make `aws help` work.
FROM console-base AS console-cli

USER root
RUN apk add --no-cache aws-cli aws-cli-doc mandoc
USER tinystack

# The default target, last so a plain docker build picks it.
FROM console-base AS console-image
