# Startup and size

These are reference measurements, not targets or guarantees. They give a baseline to compare a change against.
They were recorded on 2026-10-29 at commit `463251c6`. The machine was an Apple M4 Pro (12 cores, 24 GB) running
macOS, Go 1.27.3 and Docker 29.2.1, with arm64 builds.

## Binary size

| Build | Command | Size |
| --- | --- | --- |
| Headless | `task build:headless` | 79.3 MB |
| With console | `task build` | 82.6 MB |

Both builds are stripped (`-trimpath -ldflags="-s -w"`). The console adds its 1.7 MB of built assets plus their
embedding.

## Image size

| Image | Target | Size on disk | Content size |
| --- | --- | --- | --- |
| Headless | `docker build --target headless .` | 138 MB | 37.1 MB |
| With console | `docker build .` | 143 MB | 39.0 MB |

"Size on disk" is what `docker image ls` reports. "Content size" is what `docker image inspect` reports: the
compressed layers that are pulled. The `console-cli` target adds the AWS CLI and comes to about 350 MB.

## Startup time

Startup time is measured from launching `tinystack start` to the first successful `GET /_tinystack/health`, which
is polled every 5 ms. The figures are the median of 10 runs on a warm file cache.

| Mode | Startup | Idle memory (RSS) |
| --- | --- | --- |
| Headless, in-memory storage (`--console-port 0`) | 95 ms | 65 MB |
| With console, in-memory storage | 93 ms | 69 MB |
| Headless, `storage.mode: persistent`, empty state | 94 ms | 68 MB |
| Headless image, `docker run` to healthy | 145 ms | |

The first launch of a newly built binary on macOS takes about 3 s while the system checks it. Later launches don't
pay this cost. The first `docker run` of a new image took about 280 ms.

## Repeating the measurements

```sh
go build -trimpath -ldflags="-s -w" -o /tmp/tinystack ./cmd/tinystack
/tmp/tinystack start --port 4700 --console-port 0 &
until curl -sf -o /dev/null http://127.0.0.1:4700/_tinystack/health; do sleep 0.005; done
ps -o rss= -p $!
kill $!
```

Use a free port, so that the measurement doesn't collide with a TinyStack that is already running.
