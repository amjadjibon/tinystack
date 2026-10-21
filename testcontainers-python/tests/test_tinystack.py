"""Tests for testcontainers_tinystack."""

import http.server
import json
import os
from pathlib import Path
import subprocess
import threading
import time
import urllib.request

import docker
import pytest
from testcontainers_tinystack import TinyStackContainer

# Skip the whole module cleanly when Docker isn't reachable.
try:
    _docker_client = docker.from_env()
    if not _docker_client.ping():
        pytest.skip("Docker daemon is not responsive", allow_module_level=True)
except Exception as exc:  # noqa: BLE001
    pytest.skip(f"Docker is not reachable: {exc}", allow_module_level=True)


@pytest.fixture(scope="module")
def tinystack_image():
    image = os.environ.get("TINYSTACK_IMAGE")
    if image:
        yield image
        return

    # Build the current checkout, as the Go module's tests do, and remove the image afterwards.
    repo_root = Path(__file__).resolve().parents[2]
    image_tag = f"tinystack-test:{int(time.time() * 1000)}"
    subprocess.run(
        ["docker", "build", "--target", "headless", "-t", image_tag, str(repo_root)],
        check=True,
        capture_output=True,
    )
    yield image_tag
    subprocess.run(["docker", "image", "rm", image_tag], check=False, capture_output=True)


@pytest.fixture(scope="module")
def tinystack(tinystack_image: str):
    container = TinyStackContainer(image=tinystack_image)
    with container:
        yield container


def test_health_endpoint(tinystack: TinyStackContainer) -> None:
    endpoint = tinystack.get_endpoint_url()
    health_url = f"{endpoint}/_tinystack/health"
    with urllib.request.urlopen(health_url, timeout=10) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
        assert data.get("status") == "ok"
        assert "services" in data
        assert "storage" in data


def test_s3_sqs_lifecycle_and_reset(tinystack: TinyStackContainer) -> None:
    s3 = tinystack.get_client("s3")
    sqs = tinystack.get_client("sqs")

    bucket_name = "testcontainers-python-bucket"
    queue_name = "testcontainers-python-queue"

    # Create S3 bucket and SQS queue
    s3.create_bucket(Bucket=bucket_name)
    buckets = [b["Name"] for b in s3.list_buckets().get("Buckets", [])]
    assert bucket_name in buckets

    create_res = sqs.create_queue(QueueName=queue_name)
    queue_url = create_res["QueueUrl"]
    queues = sqs.list_queues().get("QueueUrls", [])
    assert queue_url in queues

    # Call reset()
    tinystack.reset()

    # Assert bucket and queue are gone
    after_buckets = [b["Name"] for b in s3.list_buckets().get("Buckets", [])]
    assert bucket_name not in after_buckets

    after_queues = sqs.list_queues().get("QueueUrls", [])
    assert queue_url not in after_queues


def test_reset_with_init(tinystack: TinyStackContainer) -> None:
    tinystack.reset(init=True)


def test_missing_image_raises_error(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("TINYSTACK_IMAGE", raising=False)
    with pytest.raises(ValueError, match="An image must be specified"):
        TinyStackContainer(image=None)


def test_reset_raises_on_http_error(monkeypatch: pytest.MonkeyPatch) -> None:
    class ErrorHandler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            self.send_response(500)
            self.end_headers()
            self.wfile.write(b"reset failed")

        def log_message(self, format, *args):
            pass

    server = http.server.HTTPServer(("127.0.0.1", 0), ErrorHandler)
    server_thread = threading.Thread(target=server.handle_request, daemon=True)
    server_thread.start()

    port = server.server_address[1]
    container = TinyStackContainer(image="tinystack:test")
    monkeypatch.setattr(container, "get_endpoint_url", lambda: f"http://127.0.0.1:{port}")
    with pytest.raises(RuntimeError, match="500 Internal Server Error: reset failed"):
        container.reset()
    server.server_close()


def test_get_client_requires_boto3(tinystack: TinyStackContainer, monkeypatch: pytest.MonkeyPatch) -> None:
    import sys

    monkeypatch.setitem(sys.modules, "boto3", None)
    with pytest.raises(RuntimeError, match="boto3 is required"):
        tinystack.get_client("s3")
