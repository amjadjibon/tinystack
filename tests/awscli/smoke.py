"""Build, start, and exercise TinyStack using the installed AWS CLI."""
import json
import os
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

def docker_available():
    try:
        return subprocess.run(["docker", "info"], capture_output=True, timeout=20).returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False

def main():
    docker = docker_available()
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
        # Verify signatures, so every AWS CLI call also checks botocore's SigV4 signing.
        config = Path(directory) / "tinystack.yaml"
        config.write_text("identity:\n  mode: verify\n  secret_keys:\n    test: test\n")
        with open(Path(directory) / "server.log", "w+") as log:
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
                def aws(*args, service="dynamodb"):
                    result = subprocess.run(["aws", "--endpoint-url", endpoint, "--region", "us-east-1", "--output", "json", service, *args], env=env, text=True, capture_output=True, check=True, timeout=20)
                    return json.loads(result.stdout or "{}")
                forged = subprocess.run(["aws", "--endpoint-url", endpoint, "--region", "us-east-1", "dynamodb", "list-tables"], env={**env, "AWS_SECRET_ACCESS_KEY": "wrong"}, text=True, capture_output=True, timeout=20)
                assert forged.returncode != 0 and "InvalidSignatureException" in forged.stderr, forged.stderr
                caller = aws("get-caller-identity", service="sts")
                assert caller["Account"] == "000000000000" and caller["Arn"] == "arn:aws:iam::000000000000:root", caller
                aws("create-table", "--table-name", "users", "--attribute-definitions", "AttributeName=PK,AttributeType=S", "--key-schema", "AttributeName=PK,KeyType=HASH", "--billing-mode", "PAY_PER_REQUEST")
                assert aws("list-tables")["TableNames"] == ["users"]
                item = {"PK": {"S": "one"}, "name": {"S": "TinyStack"}}
                aws("put-item", "--table-name", "users", "--item", json.dumps(item))
                assert aws("get-item", "--table-name", "users", "--key", '{"PK":{"S":"one"}}')["Item"] == item
                assert aws("scan", "--table-name", "users")["Count"] == 1
                updated = aws("update-item", "--table-name", "users", "--key", '{"PK":{"S":"one"}}', "--update-expression", "ADD visits :one", "--condition-expression", "attribute_exists(PK)", "--expression-attribute-values", '{":one":{"N":"1"}}', "--return-values", "UPDATED_NEW")
                assert updated["Attributes"] == {"visits": {"N": "1"}}, updated
                queried = aws("query", "--table-name", "users", "--key-condition-expression", "PK = :pk", "--projection-expression", "visits", "--expression-attribute-values", '{":pk":{"S":"one"}}')
                assert queried["Items"] == [{"visits": {"N": "1"}}], queried
                aws("delete-item", "--table-name", "users", "--key", '{"PK":{"S":"one"}}')
                aws("delete-table", "--table-name", "users")
                assert aws("list-tables")["TableNames"] == []
                def sqs(*args):
                    return aws(*args, service="sqs")
                queue_url = sqs("create-queue", "--queue-name", "jobs")["QueueUrl"]
                assert sqs("get-queue-url", "--queue-name", "jobs")["QueueUrl"] == queue_url
                sent = sqs("send-message", "--queue-url", queue_url, "--message-body", "hello", "--message-attributes", '{"kind":{"DataType":"String","StringValue":"test"}}')
                received = sqs("receive-message", "--queue-url", queue_url, "--message-attribute-names", "All", "--wait-time-seconds", "1")["Messages"]
                assert [m["MessageId"] for m in received] == [sent["MessageId"]]
                assert received[0]["MessageAttributes"]["kind"]["StringValue"] == "test"
                sqs("delete-message", "--queue-url", queue_url, "--receipt-handle", received[0]["ReceiptHandle"])
                assert "Messages" not in sqs("receive-message", "--queue-url", queue_url)
                sqs("delete-queue", "--queue-url", queue_url)
                assert "QueueUrls" not in sqs("list-queues")
                def sns(*args):
                    return aws(*args, service="sns")
                topic_arn = sns("create-topic", "--name", "alerts")["TopicArn"]
                assert [t["TopicArn"] for t in sns("list-topics")["Topics"]] == [topic_arn]
                inbox_url = sqs("create-queue", "--queue-name", "inbox")["QueueUrl"]
                inbox_arn = sqs("get-queue-attributes", "--queue-url", inbox_url, "--attribute-names", "QueueArn")["Attributes"]["QueueArn"]
                subscription_arn = sns("subscribe", "--topic-arn", topic_arn, "--protocol", "sqs", "--notification-endpoint", inbox_arn, "--attributes", "RawMessageDelivery=true")["SubscriptionArn"]
                published = sns("publish", "--topic-arn", topic_arn, "--message", "fan out")
                assert published["MessageId"]
                delivered = sqs("receive-message", "--queue-url", inbox_url, "--wait-time-seconds", "1")["Messages"]
                assert [m["Body"] for m in delivered] == ["fan out"]
                sns("unsubscribe", "--subscription-arn", subscription_arn)
                sns("delete-topic", "--topic-arn", topic_arn)
                assert sns("list-topics").get("Topics", []) == []
                def s3api(*args):
                    return aws(*args, service="s3api")
                s3api("create-bucket", "--bucket", "assets")
                assert [b["Name"] for b in s3api("list-buckets")["Buckets"]] == ["assets"]
                source = Path(directory) / "hello.txt"
                source.write_text("hello from the cli")
                put = s3api("put-object", "--bucket", "assets", "--key", "docs/hello world.txt", "--body", str(source), "--content-type", "text/plain")
                listed = s3api("list-objects-v2", "--bucket", "assets", "--prefix", "docs/")
                assert [(o["Key"], o["ETag"]) for o in listed["Contents"]] == [("docs/hello world.txt", put["ETag"])]
                target = Path(directory) / "downloaded.txt"
                got = s3api("get-object", "--bucket", "assets", "--key", "docs/hello world.txt", str(target))
                assert got["ContentType"] == "text/plain" and target.read_text() == "hello from the cli"
                def cli(*args):
                    return subprocess.run(["aws", "--endpoint-url", endpoint, "--region", "us-east-1", *args], env=env, text=True, capture_output=True, check=True, timeout=20).stdout
                cli("s3", "cp", str(source), "s3://assets/copied.txt")
                assert "copied.txt" in cli("s3", "ls", "s3://assets/")
                cli("s3", "rm", "s3://assets/copied.txt")
                s3api("delete-object", "--bucket", "assets", "--key", "docs/hello world.txt")
                s3api("delete-bucket", "--bucket", "assets")
                assert s3api("list-buckets").get("Buckets", []) == []
                def events(*args):
                    return aws(*args, service="events")
                rule_arn = events("put-rule", "--name", "orders", "--event-pattern", '{"source":["shop"]}')["RuleArn"]
                assert rule_arn.endswith(":rule/orders")
                events_url = sqs("create-queue", "--queue-name", "events-inbox")["QueueUrl"]
                events_arn = sqs("get-queue-attributes", "--queue-url", events_url, "--attribute-names", "QueueArn")["Attributes"]["QueueArn"]
                events("put-targets", "--rule", "orders", "--targets", json.dumps([{"Id": "inbox", "Arn": events_arn, "InputPath": "$.detail"}]))
                put_events = events("put-events", "--entries", json.dumps([{"Source": "shop", "DetailType": "order", "Detail": '{"id":7}'}]))
                assert put_events["FailedEntryCount"] == 0
                routed = sqs("receive-message", "--queue-url", events_url, "--wait-time-seconds", "1")["Messages"]
                assert [json.loads(m["Body"]) for m in routed] == [{"id": 7}]
                events("remove-targets", "--rule", "orders", "--ids", "inbox")
                events("delete-rule", "--name", "orders")
                assert events("list-rules").get("Rules", []) == []
                def ssm(*args):
                    return aws(*args, service="ssm")
                assert ssm("put-parameter", "--name", "/smoke/url", "--value", "db.local", "--type", "String")["Version"] == 1
                ssm("put-parameter", "--name", "/smoke/token", "--value", "s3cret", "--type", "SecureString")
                by_path = ssm("get-parameters-by-path", "--path", "/smoke", "--with-decryption")["Parameters"]
                assert [(p["Name"], p["Value"]) for p in by_path] == [("/smoke/token", "s3cret"), ("/smoke/url", "db.local")], by_path
                assert ssm("get-parameter", "--name", "/smoke/token")["Parameter"]["Value"] != "s3cret"
                assert ssm("delete-parameters", "--names", "/smoke/url", "/smoke/token")["DeletedParameters"] == ["/smoke/url", "/smoke/token"]
                def secrets(*args):
                    return aws(*args, service="secretsmanager")
                secret_arn = secrets("create-secret", "--name", "smoke/db", "--secret-string", '{"password":"one"}')["ARN"]
                secrets("put-secret-value", "--secret-id", secret_arn, "--secret-string", '{"password":"two"}')
                assert secrets("get-secret-value", "--secret-id", "smoke/db")["SecretString"] == '{"password":"two"}'
                assert secrets("get-secret-value", "--secret-id", "smoke/db", "--version-stage", "AWSPREVIOUS")["SecretString"] == '{"password":"one"}'
                assert [e["Name"] for e in secrets("list-secrets")["SecretList"]] == ["smoke/db"]
                secrets("delete-secret", "--secret-id", "smoke/db", "--force-delete-without-recovery")
                def iam(*args):
                    return aws(*args, service="iam")
                trust = {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": {"Service": "lambda.amazonaws.com"}, "Action": "sts:AssumeRole"}]}
                role = iam("create-role", "--role-name", "lambda", "--assume-role-policy-document", json.dumps(trust))["Role"]
                # IAM returns policy documents URL-encoded; the AWS CLI decodes them.
                assert iam("get-role", "--role-name", "lambda")["Role"]["AssumeRolePolicyDocument"] == trust
                iam("attach-role-policy", "--role-name", "lambda", "--policy-arn", "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole")
                assert [p["PolicyName"] for p in iam("list-attached-role-policies", "--role-name", "lambda")["AttachedPolicies"]] == ["AWSLambdaBasicExecutionRole"]
                def lam(*args):
                    return aws(*args, service="lambda")
                package = Path(directory) / "function.zip"
                with zipfile.ZipFile(package, "w") as archive:
                    archive.writestr("app.py", "def handler(event, context):\n    return {'echo': event}\n")
                created = lam("create-function", "--function-name", "hello", "--runtime", "python3.14", "--handler", "app.handler", "--role", role["Arn"], "--zip-file", f"fileb://{package}")
                assert created["State"] == "Active" and created["Version"] == "$LATEST"
                assert lam("get-function", "--function-name", "hello")["Configuration"]["CodeSha256"] == created["CodeSha256"]
                assert [f["FunctionName"] for f in lam("list-functions")["Functions"]] == ["hello"]
                if docker:
                    output, payload = Path(directory) / "out.json", Path(directory) / "payload.json"
                    payload.write_text('{"n":1}')
                    # fileb:// sends the payload unchanged with AWS CLI v1 and v2.
                    invoked = lam("invoke", "--function-name", "hello", "--payload", f"fileb://{payload}", str(output))
                    assert invoked["StatusCode"] == 200 and "FunctionError" not in invoked
                    assert json.loads(output.read_text()) == {"echo": {"n": 1}}
                lam("delete-function", "--function-name", "hello")
                assert lam("list-functions").get("Functions", []) == []
                iam("detach-role-policy", "--role-name", "lambda", "--policy-arn", "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole")
                iam("delete-role", "--role-name", "lambda")
                deployer_trust = {"Version": "2012-10-17", "Statement": [{"Effect": "Allow", "Principal": {"AWS": "000000000000"}, "Action": "sts:AssumeRole"}]}
                deployer = iam("create-role", "--role-name", "deployer", "--assume-role-policy-document", json.dumps(deployer_trust))["Role"]
                session = aws("assume-role", "--role-arn", deployer["Arn"], "--role-session-name", "smoke", service="sts")["Credentials"]
                # Temporary credentials sign requests, verified against the session's secret and token.
                session_env = {**env, "AWS_ACCESS_KEY_ID": session["AccessKeyId"], "AWS_SECRET_ACCESS_KEY": session["SecretAccessKey"], "AWS_SESSION_TOKEN": session["SessionToken"]}
                assumed = subprocess.run(["aws", "--endpoint-url", endpoint, "--region", "us-east-1", "--output", "json", "sts", "get-caller-identity"], env=session_env, text=True, capture_output=True, check=True, timeout=20)
                assert json.loads(assumed.stdout)["Arn"] == "arn:aws:sts::000000000000:assumed-role/deployer/smoke", assumed.stdout
                iam("delete-role", "--role-name", "deployer")
                assert iam("list-roles")["Roles"] == []
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
        if docker:
            left = subprocess.run(["docker", "ps", "-aq", "--filter", "label=tinystack.function"], text=True, capture_output=True, check=True).stdout
            assert left.strip() == "", f"function containers left after shutdown: {left}"
        print("AWS CLI smoke passed; SIGTERM shutdown passed." + ("" if docker else " Lambda invoke skipped: Docker is unavailable."))

if __name__ == "__main__":
    main()
