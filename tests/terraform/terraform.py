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
        # Each team deploys different code, so the update also replaces the function's code.
        packages = {}
        for team in ("core", "platform"):
            packages[team] = work / f"function-{team}.zip"
            with zipfile.ZipFile(packages[team], "w") as archive:
                archive.writestr("app.py", f"TEAM = {team!r}\n\ndef handler(event, context):\n    return event\n")
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        endpoint = f"http://127.0.0.1:{port}"
        # Verify signatures, so every call also checks the provider's SigV4 signing.
        config = work / "tinystack.yaml"
        config.write_text("identity:\n  mode: verify\n  secret_keys:\n    test: test\n")

        def terraform(*args, team="core", code=0):
            command = ["terraform", args[0], "-no-color"]
            if args[0] != "output":
                command.append("-input=false")
            if args[0] not in ("init", "output"):
                command += ["-var", f"endpoint={endpoint}", "-var", f"function_zip={packages[team]}", "-var", f"team={team}"]
            command += args[1:]
            result = subprocess.run(command, cwd=work, text=True, capture_output=True, timeout=600)
            if result.returncode != code:
                raise RuntimeError(f"terraform {args[0]} exited {result.returncode}:\n{result.stdout}\n{result.stderr}")
            if args[0] in ("apply", "plan", "destroy"):
                for line in result.stdout.splitlines():
                    if line.startswith(("Apply complete!", "Destroy complete!", "No changes.", "Plan:")):
                        print(line, flush=True)
            return result.stdout

        with open(work / "server.log", "w+") as log:
            process = subprocess.Popen([str(binary), "start", "--port", str(port), "--console-port", "0", "--config", str(config)], stdout=log, stderr=log)
            try:
                # Allow startup under concurrent worktree builds and race-test load.
                for _ in range(600):
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
                fixture = {"target": {"workgroupName": "local-warehouse"}, "database": "dev",
                           "contains": "select tinystack_fixture", "columnMetadata": [{"name": "answer"}],
                           "records": [[{"stringValue": "user fixture"}]]}
                request = urllib.request.Request(endpoint + "/_tinystack/redshift-data/fixtures/terraform",
                                                 data=json.dumps(fixture).encode(), method="PUT",
                                                 headers={"Content-Type": "application/json"})
                with urllib.request.urlopen(request, timeout=5) as response:
                    assert response.status == 200
                terraform("init")
                terraform("apply", "-auto-approve")
                assert terraform("output", "-raw", "vpc_endpoint_service") == "com.amazonaws.us-east-1.s3"
                assert terraform("output", "-raw", "account_id") == "000000000000"
                assert terraform("output", "-raw", "zones") == "3"
                assert terraform("output", "-raw", "image_matches") == "true"
                assert terraform("output", "-raw", "registry_user") == "AWS"
                assert terraform("output", "-raw", "iot_endpoint").endswith("-ats.iot.us-east-1.localhost")
                # Remove only the state binding, then import the unchanged CUR definition.
                subprocess.run(["terraform", "state", "rm", "aws_cur_report_definition.local"],
                               cwd=work, check=True, capture_output=True, text=True)
                terraform("import", "aws_cur_report_definition.local", "local-costs")
                # -detailed-exitcode returns 0 only when the plan has no changes.
                terraform("plan", "-detailed-exitcode")
                # OpenSearch and Flink add two updates, DMS adds five, Signer profile tags add one, WAF Classic adds four,
                # EMR cluster tags and task-group capacity add two, Amplify app/branch updates add two,
                # Access Analyzer tags add one, Security Hub action target and insight names add two, and
                # Network Firewall rule group, policy and firewall tags add three; Inspector filter tags add one;
                # Resource Explorer index and view tags add two; Service Catalog portfolio and product tags add two.
                # Global Accelerator accelerator tags add one; its listener and endpoint group stay unchanged.
                # FIS experiment-template tags force replacement in provider 6.x: one extra add and destroy.
                # Shield protection and protection-group tags update in place: +2 changes.
                # DataSync IAM role, two S3 locations and task tags add four.
                # SWF domain tags update in place (+1); name, description and retention are ForceNew and unchanged.
                # DSQL three cluster tag sets update in place (+3); witness_region is ForceNew and unchanged.
                # Both peering resources are unchanged and have no tags.
                # MediaConvert queue tags update in place (+1); name/pricing_plan are ForceNew and unchanged.
                # Glue job, trigger and workflow tags update in place (+3); ForceNew names/types are unchanged.
                # Glue crawler tags update in place (+1); classifier has no tags and its name is unchanged.
                # Transcribe vocabulary/filter tags update in place (+2); ForceNew names/languages stay unchanged.
                # Bedrock agent, alias and knowledge-base tags update in place (+3 changes).
                # Agent role, alias agent_id and KB storage_configuration are ForceNew and unchanged;
                # the action group has no tags and none of its properties changes.
                # AgentCore runtime description/environment/tags, runtime endpoint tags,
                # and memory tags update in place (+3 changes); names, roles, protocol,
                # artifact and memory event expiry remain unchanged.
                # CloudFront KVS tags and its key value update in place (+2); names/ARN/key are unchanged.
                # Redshift Serverless namespace and workgroup tags update in place (+2).
                # Snapshot, usage limit and Data API statement have no team-dependent attributes.
                # S3 access point tags update in place (+1); its bucket/name/VPC/BPA are ForceNew.
                # The account BPA booleans and policy stay fixed; MRAP details are ForceNew and untaggable.
                # The Timestream Query scheduled query has fixed tags (see main.tf) and does not change.
                # S3 Files file-system and access-point tags update in place (+2); other properties stay fixed.
                updated = terraform("apply", "-auto-approve", team="platform")
                assert "8 added, 156 changed, 8 destroyed" in updated, updated
                assert terraform("output", "-raw", "stack_parameter") == "team-platform"
                terraform("plan", "-detailed-exitcode", team="platform")
                # The provider replaces the monitor when its name changes. The subscription
                # and category update in place: +1 replacement and +2 changes vs the baseline.
                state = json.loads(subprocess.run(
                    ["terraform", "show", "-json"], cwd=work, text=True,
                    capture_output=True, check=True,
                ).stdout)
                resources = {r["address"]: r for r in state["values"]["root_module"]["resources"]}
                for address in ("aws_ce_anomaly_monitor.services", "aws_ce_anomaly_subscription.daily", "aws_ce_cost_category.team"):
                    resource_id = resources[address]["values"]["id"]
                    subprocess.run(["terraform", "state", "rm", address], cwd=work, check=True, capture_output=True)
                    terraform("import", address, resource_id, team="platform")
                for kind in ("file_system", "mount_target", "access_point", "file_system_policy",
                             "synchronization_configuration"):
                    address = f"aws_s3files_{kind}.local"
                    values = resources[address]["values"]
                    resource_id = values["file_system_id"] if kind in (
                        "file_system_policy", "synchronization_configuration") else values["id"]
                    subprocess.run(["terraform", "state", "rm", address], cwd=work, check=True, capture_output=True)
                    terraform("import", address, resource_id, team="platform")
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
