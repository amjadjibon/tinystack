# Testcontainers for TinyStack (Python)

A Python module for [Testcontainers](https://testcontainers-python.readthedocs.io/) that starts [TinyStack](https://github.com/amjadjibon/tinystack) for integration tests.

## Installation

```sh
pip install testcontainers-tinystack
```

With optional `boto3` support:

```sh
pip install 'testcontainers-tinystack[boto3]'
```

## Build an image first

The registry image `ghcr.io/amjadjibon/tinystack:latest` is **not published yet**. Build a local image from the repository root:

```sh
docker build --target headless -t tinystack:test .
```

Once published, the registry image can be passed directly to `TinyStackContainer`:

```python
from testcontainers_tinystack import TinyStackContainer

with TinyStackContainer("ghcr.io/amjadjibon/tinystack:latest") as tinystack:
    ...
```

## Usage

```python
from testcontainers_tinystack import TinyStackContainer

# Explicitly pass the image tag or set TINYSTACK_IMAGE in your environment
with TinyStackContainer("tinystack:test") as tinystack:
    endpoint = tinystack.get_endpoint_url()
    
    # Optional boto3 client helper (dummy credentials test/test):
    s3 = tinystack.get_client("s3")
    s3.create_bucket(Bucket="my-bucket")

    # Clear state between test cases without restarting the container:
    tinystack.reset()
```

### Environment Variable `TINYSTACK_IMAGE`

If no `image` argument is passed to `TinyStackContainer()`, the container defaults to the `TINYSTACK_IMAGE` environment variable:

```sh
export TINYSTACK_IMAGE=tinystack:test
```

```python
with TinyStackContainer() as tinystack:
    s3 = tinystack.get_client("s3")
```

If neither `image` nor `TINYSTACK_IMAGE` is provided, `TinyStackContainer` raises a `ValueError`.

### Container Configuration

`TinyStackContainer` exposes port `4566` and starts with the command `start --host 0.0.0.0 --port 4566`.
Startup waits up to 120 seconds for `GET /_tinystack/health` to return HTTP 200.

- `get_endpoint_url()` returns `http://<host>:<mapped port>`.
- `get_client(service_name, region_name="us-east-1", **kwargs)` returns a boto3 client pointing to the container endpoint with dummy credentials (`test`/`test`). Extra keyword arguments are forwarded to `boto3.client()`.
- `reset(init=False)` sends `POST /_tinystack/reset` with a 60-second timeout, clearing all emulated AWS service state. Pass `init=True` (`POST /_tinystack/reset?init=1`) to re-run ready-stage init scripts after clearing state. Non-2xx responses raise a `RuntimeError`.

## Run the test suite

```sh
task test:testcontainers:python
```

The tests use the image specified by `TINYSTACK_IMAGE` or build a local headless image using `docker build`. Tests skip cleanly when Docker is unavailable.
