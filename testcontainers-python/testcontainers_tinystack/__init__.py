"""TinyStack container for Testcontainers Python."""

from __future__ import annotations

import os
from typing import Any
import urllib.error
import urllib.request

from testcontainers.core.container import DockerContainer
from testcontainers.core.wait_strategies import HttpWaitStrategy

AWS_PORT = 4566


class TinyStackContainer(DockerContainer):
    """TinyStack Testcontainers implementation for AWS integration testing."""

    def __init__(
        self,
        image: str | None = None,
        **kwargs: Any,
    ) -> None:
        resolved_image = image or os.environ.get("TINYSTACK_IMAGE")
        if not resolved_image:
            raise ValueError(
                "An image must be specified either via the 'image' argument "
                "or the TINYSTACK_IMAGE environment variable"
            )
        super().__init__(image=resolved_image, **kwargs)
        self.with_exposed_ports(AWS_PORT)
        self.with_command(f"start --host 0.0.0.0 --port {AWS_PORT}")
        self.waiting_for(
            HttpWaitStrategy(AWS_PORT, "/_tinystack/health")
            .for_status_code(200)
            .with_startup_timeout(120)
        )

    def get_endpoint_url(self) -> str:
        """Return the host-accessible endpoint URL including the HTTP scheme."""
        host = self.get_container_host_ip()
        port = self.get_exposed_port(AWS_PORT)
        return f"http://{host}:{port}"

    def reset(self, init: bool = False) -> None:
        """Clear TinyStack's state via POST /_tinystack/reset.

        If init is True, the ready stage scripts will be re-run (?init=1).
        Raises RuntimeError on non-2xx response.
        """
        endpoint = self.get_endpoint_url()
        path = "/_tinystack/reset?init=1" if init else "/_tinystack/reset"
        url = f"{endpoint}{path}"
        req = urllib.request.Request(url, data=b"", method="POST")
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                if resp.status < 200 or resp.status >= 300:
                    body = resp.read(4096).decode("utf-8", errors="replace")
                    raise RuntimeError(f"reset tinystack: {resp.status} {resp.reason}: {body}")
        except urllib.error.HTTPError as exc:
            body = exc.read(4096).decode("utf-8", errors="replace")
            raise RuntimeError(f"reset tinystack: {exc.code} {exc.reason}: {body}") from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(f"reset tinystack: {exc.reason}") from exc

    def get_client(
        self,
        service_name: str,
        region_name: str = "us-east-1",
        **kwargs: Any,
    ) -> Any:
        """Create a boto3 client pointing to the TinyStack container with dummy credentials."""
        try:
            import boto3
        except ImportError as exc:
            raise RuntimeError(
                "boto3 is required for get_client(). Install it with: "
                "pip install testcontainers-tinystack[boto3]"
            ) from exc

        client_kwargs: dict[str, Any] = {
            "endpoint_url": self.get_endpoint_url(),
            "region_name": region_name,
            "aws_access_key_id": "test",
            "aws_secret_access_key": "test",
        }
        client_kwargs.update(kwargs)
        return boto3.client(service_name, **client_kwargs)


__all__ = ["TinyStackContainer", "AWS_PORT"]
