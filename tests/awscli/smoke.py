"""Build, start, and exercise TinyStack using the installed AWS CLI."""
import json
import os
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

def main():
    with tempfile.TemporaryDirectory(prefix="tinystack-smoke-") as directory:
        binary = Path(directory) / "tinystack"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/tinystack"], cwd=ROOT, check=True)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        endpoint = f"http://127.0.0.1:{port}"
        env = os.environ.copy()
        env.update(AWS_ACCESS_KEY_ID="test", AWS_SECRET_ACCESS_KEY="test", AWS_DEFAULT_REGION="us-east-1", AWS_EC2_METADATA_DISABLED="true", AWS_PAGER="")
        env.pop("AWS_SESSION_TOKEN", None)
        with open(Path(directory) / "server.log", "w+") as log:
            process = subprocess.Popen([str(binary), "start", "--port", str(port)], stdout=log, stderr=log)
            try:
                for _ in range(100):
                    if process.poll() is not None:
                        raise RuntimeError("server exited during startup")
                    try:
                        with urllib.request.urlopen(endpoint + "/_tinystack/health", timeout=0.2) as response:
                            assert json.load(response)["status"] == "ok"
                        break
                    except (urllib.error.URLError, TimeoutError):
                        time.sleep(0.05)
                else:
                    raise RuntimeError("server never became healthy")
                def aws(*args):
                    result = subprocess.run(["aws", "--endpoint-url", endpoint, "--region", "us-east-1", "--output", "json", "dynamodb", *args], env=env, text=True, capture_output=True, check=True, timeout=20)
                    return json.loads(result.stdout or "{}")
                aws("create-table", "--table-name", "users", "--attribute-definitions", "AttributeName=PK,AttributeType=S", "--key-schema", "AttributeName=PK,KeyType=HASH", "--billing-mode", "PAY_PER_REQUEST")
                assert aws("list-tables")["TableNames"] == ["users"]
                item = {"PK": {"S": "one"}, "name": {"S": "TinyStack"}}
                aws("put-item", "--table-name", "users", "--item", json.dumps(item))
                assert aws("get-item", "--table-name", "users", "--key", '{"PK":{"S":"one"}}')["Item"] == item
                assert aws("scan", "--table-name", "users")["Count"] == 1
                aws("delete-item", "--table-name", "users", "--key", '{"PK":{"S":"one"}}')
                aws("delete-table", "--table-name", "users")
                assert aws("list-tables")["TableNames"] == []
            finally:
                if process.poll() is None:
                    process.terminate()
                try:
                    process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                    raise
                if process.returncode != 0:
                    log.seek(0)
                    raise RuntimeError(log.read())
        print("AWS CLI smoke passed; SIGTERM shutdown passed.")

if __name__ == "__main__":
    main()
