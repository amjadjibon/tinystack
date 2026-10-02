"""Build and start TinyStack, then deploy, update, and delete tests/sam/template.yaml with the AWS SAM CLI.

Needs the SAM CLI and the AWS CLI. The APIs are called, which runs the function, only when Docker is available.
"""
import json
import os
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
STACK = "tinystack-sam"
BUCKET = "sam-artifacts"


def docker_available():
    try:
        return subprocess.run(["docker", "info"], capture_output=True, timeout=20).returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


def main():
    docker = docker_available()
    with tempfile.TemporaryDirectory(prefix="tinystack-sam-") as directory:
        work = Path(directory)
        binary = work / "tinystack"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/tinystack"], cwd=ROOT, check=True)
        project = work / "project"
        shutil.copytree(ROOT / "tests" / "sam", project, ignore=shutil.ignore_patterns("sam.py", ".aws-sam"))
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        endpoint = f"http://127.0.0.1:{port}"
        env = os.environ.copy()
        env.update(
            AWS_ACCESS_KEY_ID="test", AWS_SECRET_ACCESS_KEY="test", AWS_DEFAULT_REGION="us-east-1",
            AWS_ENDPOINT_URL=endpoint, AWS_EC2_METADATA_DISABLED="true", AWS_PAGER="", SAM_CLI_TELEMETRY="0",
        )
        env.pop("AWS_SESSION_TOKEN", None)
        env.pop("AWS_PROFILE", None)
        # Verify signatures, so every call also checks botocore's SigV4 signing.
        config = work / "tinystack.yaml"
        config.write_text("identity:\n  mode: verify\n  secret_keys:\n    test: test\n")

        def run(*command, timeout=300):
            result = subprocess.run(command, cwd=project, env=env, text=True, capture_output=True, timeout=timeout)
            if result.returncode != 0:
                raise RuntimeError(f"{' '.join(command[:2])} exited {result.returncode}:\n{result.stdout}\n{result.stderr}")
            return result.stdout

        def aws(*args):
            return json.loads(run("aws", "--output", "json", *args) or "{}")

        def deploy(*overrides):
            command = [
                "sam", "deploy", "--stack-name", STACK, "--s3-bucket", BUCKET, "--region", "us-east-1",
                "--capabilities", "CAPABILITY_IAM", "--no-confirm-changeset", "--no-fail-on-empty-changeset",
            ]
            if overrides:
                command += ["--parameter-overrides", *overrides]
            run(*command)
            stack = aws("cloudformation", "describe-stacks", "--stack-name", STACK)["Stacks"][0]
            assert stack["StackStatus"] in ("CREATE_COMPLETE", "UPDATE_COMPLETE"), stack["StackStatus"]
            return {o["OutputKey"]: o["OutputValue"] for o in stack["Outputs"]}

        def get(url):
            with urllib.request.urlopen(url, timeout=120) as response:
                assert response.status == 200, response.status
                return json.load(response)

        def alias_version(outputs):
            name, alias = outputs["Alias"].rsplit(":", 1)
            return aws("lambda", "get-alias", "--function-name", name, "--name", alias)["FunctionVersion"]

        with open(work / "server.log", "w+") as log:
            process = subprocess.Popen([str(binary), "start", "--port", str(port), "--config", str(config)], stdout=log, stderr=log)
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
                aws("s3api", "create-bucket", "--bucket", BUCKET)

                outputs = deploy()
                assert alias_version(outputs) == "1"
                table = aws("dynamodb", "describe-table", "--table-name", outputs["Table"])["Table"]
                assert table["KeySchema"] == [{"AttributeName": "orderId", "KeyType": "HASH"}], table
                function = outputs["Alias"]
                mappings = aws("lambda", "list-event-source-mappings", "--function-name", function)["EventSourceMappings"]
                assert [m["BatchSize"] for m in mappings] == [5], mappings
                processed = aws("cloudformation", "get-template", "--stack-name", STACK, "--template-stage", "Processed")
                assert "AWS::Serverless" not in json.dumps(processed["TemplateBody"])
                rest = f"{endpoint}/_aws/execute-api/{outputs['RestApi']}/Prod/orders/7"
                http = f"{endpoint}/_aws/execute-api/{outputs['HttpApi']}/orders"
                if docker:
                    assert get(rest) == {"greeting": "hello", "release": "one", "path": "/orders/7"}
                    assert get(http) == {"greeting": "hello", "release": "one", "path": "/orders"}

                # New code publishes version 2 and moves the alias, which both APIs invoke.
                app = project / "app" / "app.py"
                app.write_text(app.read_text().replace('"release": "one"', '"release": "two"'))
                outputs = deploy("Greeting=hi")
                assert alias_version(outputs) == "2"
                if docker:
                    assert get(rest) == {"greeting": "hi", "release": "two", "path": "/orders/7"}
                    assert get(http) == {"greeting": "hi", "release": "two", "path": "/orders"}

                # Nothing changed, so sam deploy makes no change set and the stack keeps its version.
                deploy("Greeting=hi")
                assert alias_version(outputs) == "2"

                run("sam", "delete", "--stack-name", STACK, "--region", "us-east-1", "--s3-bucket", BUCKET, "--no-prompts")
                stacks = aws("cloudformation", "list-stacks", "--stack-status-filter", "DELETE_COMPLETE")["StackSummaries"]
                assert [s["StackName"] for s in stacks] == [STACK], stacks
                assert aws("lambda", "list-functions").get("Functions", []) == []
            finally:
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=8)
                if process.returncode != 0:
                    log.seek(0)
                    raise RuntimeError(log.read())
    print("SAM deploy, update, and delete passed." + ("" if docker else " API calls skipped: Docker is unavailable."))


if __name__ == "__main__":
    main()
