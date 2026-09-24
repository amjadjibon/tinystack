"""Build and start TinyStack, then apply, update, and destroy tests/terraform/main.tf with Terraform.

Needs Terraform and network access to download the hashicorp/aws provider on first use.
"""
import json
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def main():
    with tempfile.TemporaryDirectory(prefix="tinystack-terraform-") as directory:
        work = Path(directory)
        binary = work / "tinystack"
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/tinystack"], cwd=ROOT, check=True)
        shutil.copy(ROOT / "tests" / "terraform" / "main.tf", work / "main.tf")
        package = work / "function.zip"
        with zipfile.ZipFile(package, "w") as archive:
            archive.writestr("app.py", "def handler(event, context):\n    return event\n")
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        endpoint = f"http://127.0.0.1:{port}"
        # Verify signatures, so every call also checks the provider's SigV4 signing.
        config = work / "tinystack.yaml"
        config.write_text("identity:\n  mode: verify\n  secret_keys:\n    test: test\n")

        def terraform(*args, team="core", code=0):
            command = ["terraform", *args, "-no-color", "-input=false"]
            if args[0] != "init":
                command += ["-var", f"endpoint={endpoint}", "-var", f"function_zip={package}", "-var", f"team={team}"]
            result = subprocess.run(command, cwd=work, text=True, capture_output=True, timeout=600)
            if result.returncode != code:
                raise RuntimeError(f"terraform {args[0]} exited {result.returncode}:\n{result.stdout}\n{result.stderr}")
            return result.stdout

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
                terraform("init")
                terraform("apply", "-auto-approve")
                # -detailed-exitcode returns 0 only when the plan has no changes.
                terraform("plan", "-detailed-exitcode")
                updated = terraform("apply", "-auto-approve", team="platform")
                assert "6 changed" in updated, updated
                terraform("plan", "-detailed-exitcode", team="platform")
                terraform("destroy", "-auto-approve", team="platform")
            finally:
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=8)
                if process.returncode != 0:
                    log.seek(0)
                    raise RuntimeError(log.read())
    print("Terraform apply, update, and destroy passed.")


if __name__ == "__main__":
    main()
