"""Build and start TinyStack, then bootstrap, deploy, change, and destroy tests/cdk/app with the AWS CDK CLI.

Needs node, npm (which installs aws-cdk-lib; the network is used for that), the CDK CLI (`cdk`), and the AWS CLI.
Runs the app as the 12-digit access key 123456789012, so the stacks land in that account, with signatures verified.
It is opt-in: `task test:cdk`.
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
ACCOUNT = "123456789012"
SECRET = "cdk-secret"
STACK = "TinyStackCdk"


def main():
    for tool in ("node", "npm", "cdk", "aws"):
        if shutil.which(tool) is None:
            raise SystemExit(f"{tool} is required for the CDK smoke test and was not found in PATH")
    with tempfile.TemporaryDirectory(prefix="tinystack-cdk-") as directory:
        work = Path(directory)
        binary = work / "tinystack"
        local = work / "cdklocal"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/tinystack"], cwd=ROOT, check=True)
        subprocess.run(["go", "build", "-o", str(local), "./cmd/cdk-local"], cwd=ROOT, check=True)
        project = work / "project"
        shutil.copytree(ROOT / "tests" / "cdk" / "app", project, ignore=shutil.ignore_patterns("node_modules", "cdk.out"))
        subprocess.run(["npm", "install", "--no-audit", "--no-fund"], cwd=project, check=True, timeout=600)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        endpoint = f"http://127.0.0.1:{port}"
        env = os.environ.copy()
        env.update(
            AWS_ACCESS_KEY_ID=ACCOUNT, AWS_SECRET_ACCESS_KEY=SECRET, AWS_DEFAULT_REGION="us-east-1",
            AWS_REGION="us-east-1", AWS_ENDPOINT_URL=endpoint, AWS_EC2_METADATA_DISABLED="true", AWS_PAGER="",
            CDK_DISABLE_VERSION_CHECK="1", CI="true",
        )
        for name in ("AWS_SESSION_TOKEN", "AWS_PROFILE", "CDK_DEFAULT_ACCOUNT", "CDK_DEFAULT_REGION"):
            env.pop(name, None)
        # Verify signatures, so the CDK CLI's SDK signing is checked too. The 12-digit key is the account.
        config = work / "tinystack.yaml"
        config.write_text(f'identity:\n  mode: verify\n  secret_keys:\n    "{ACCOUNT}": {SECRET}\n')

        def run(*command, timeout=600):
            result = subprocess.run(command, cwd=project, env=env, text=True, capture_output=True, timeout=timeout)
            if result.returncode != 0:
                raise RuntimeError(f"{' '.join(command[:3])} exited {result.returncode}:\n{result.stdout}\n{result.stderr}")
            return result.stdout + result.stderr

        def aws(*args):
            return json.loads(run("aws", "--endpoint-url", endpoint, "--output", "json", *args) or "{}")

        def outputs():
            stack = aws("cloudformation", "describe-stacks", "--stack-name", STACK)["Stacks"][0]
            assert stack["StackStatus"] in ("CREATE_COMPLETE", "UPDATE_COMPLETE"), stack["StackStatus"]
            return {o["OutputKey"]: o["OutputValue"] for o in stack["Outputs"]}

        with open(work / "server.log", "w+") as log:
            process = subprocess.Popen([str(binary), "start", "--port", str(port), "--console-port", "0", "--config", str(config)],
                                       stdout=log, stderr=log)
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

                assert aws("sts", "get-caller-identity")["Account"] == ACCOUNT

                run(str(local), "bootstrap")
                version = aws("ssm", "get-parameter", "--name", "/cdk-bootstrap/hnb659fds/version")["Parameter"]["Value"]
                assert int(version) >= 6, version
                toolkit = aws("cloudformation", "describe-stacks", "--stack-name", "CDKToolkit")["Stacks"][0]
                assert toolkit["StackStatus"] == "CREATE_COMPLETE", toolkit["StackStatus"]

                run(str(local), "deploy", "--require-approval", "never")
                out = outputs()
                assert out["QueueUrl"].endswith(f"/{ACCOUNT}/cdk-jobs"), out
                assert out["BucketName"] == f"cdk-files-{ACCOUNT}", out
                function = aws("lambda", "get-function", "--function-name", out["FunctionName"])["Configuration"]
                assert function["Environment"]["Variables"]["QUEUE_URL"] == out["QueueUrl"], function
                assert aws("sqs", "list-queues")["QueueUrls"] == [out["QueueUrl"]]
                assets = aws("s3api", "list-objects-v2", "--bucket", f"cdk-hnb659fds-assets-{ACCOUNT}-us-east-1")
                assert len(assets["Contents"]) >= 2, assets

                # A new code asset updates the function.
                (project / "handler" / "index.js").write_text("exports.handler = async () => ({ ok: 'changed' });\n")
                run(str(local), "deploy", "--require-approval", "never")
                assert outputs()["QueueUrl"] == out["QueueUrl"]

                run(str(local), "destroy", "--force")
                stacks = aws("cloudformation", "list-stacks", "--stack-status-filter", "DELETE_COMPLETE")["StackSummaries"]
                assert STACK in [s["StackName"] for s in stacks], stacks
                assert aws("sqs", "list-queues").get("QueueUrls", []) == []
            finally:
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=8)
                if process.returncode != 0:
                    log.seek(0)
                    raise RuntimeError(log.read())
    print("CDK bootstrap, deploy, update, and destroy passed.")


if __name__ == "__main__":
    main()
