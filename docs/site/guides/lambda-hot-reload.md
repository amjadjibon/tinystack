# Lambda hot reload

Use the fixed magic bucket `hot-reload` with `S3Key` set to an absolute host directory.
TinyStack stores the path, without copying a ZIP or reading an S3 object. This uses the
[LocalStack hot reload deployment contract](https://docs.localstack.cloud/aws/tooling/lambda-tools/hot-reloading/).
The bucket name is not configurable.

## AWS CLI

Start TinyStack with its Docker Lambda runtime enabled (see [Docker runtimes](runtimes.md)).
Put `app.py` in your directory:

```python
value = "first"

def handler(event, context):
    return {"value": value}
```

Create and invoke the function with local credentials:

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
CODE_DIR="$(pwd)/lambda-code"
aws --endpoint-url http://localhost:4566 lambda create-function \
  --function-name live --runtime python3.12 --handler app.handler \
  --role arn:aws:iam::000000000000:role/lambda \
  --code "S3Bucket=hot-reload,S3Key=$CODE_DIR"
aws --endpoint-url http://localhost:4566 lambda invoke --function-name live output.json
```

Change `value` in `app.py`, then invoke again. No code update API call is needed.
To switch an existing ZIP function to a directory:

```sh
aws --endpoint-url http://localhost:4566 lambda update-function-code \
  --function-name live --s3-bucket hot-reload --s3-key "$CODE_DIR"
```

Use `update-function-code --zip-file fileb://function.zip` to return to ZIP deployment.
Relative paths, missing paths, regular files, `ZipFile` alongside the magic bucket, and
`S3ObjectVersion` are rejected with `InvalidParameterValueException`.

## Terraform

With your AWS provider pointed at TinyStack, deploy the function using the same S3 fields:

```hcl
resource "aws_lambda_function" "live" {
  function_name = "live"
  role          = "arn:aws:iam::000000000000:role/lambda"
  runtime       = "python3.12"
  handler       = "app.handler"
  s3_bucket     = "hot-reload"
  s3_key        = abspath("${path.module}/lambda-code")
}
```

See [infrastructure workflows](infrastructure.md) for local provider configuration.
File edits take effect on the next invoke without `terraform apply`.

## Execution and persistence

Every supported ZIP runtime mounts the directory **read-only at `/var/task`**, the same path
used for extracted ZIP packages. Normal ZIP layers still load into `/opt`. Docker must be
able to bind-mount the host path; remote Docker daemons need that same path on their host,
and Docker Desktop needs file sharing enabled for the directory.
With Colima on macOS, keep the directory under your home directory (for example, in the
repository). The default macOS temporary directory under `/var/folders` is not shared by
Colima unless you configure an additional mount.
VM file sharing can delay an edit's visibility inside the container. Invoke after the
mount reflects the edit; restarting a warm container does not refresh the VM's filesystem cache.

Before every Docker invocation, TinyStack fingerprints the tree's relative names, sizes,
modification times and modes. A change replaces the function's warm container pool.
Idle old containers are removed; busy old containers finish their invocation and are then
removed. The next invocation starts a fresh container, so interpreted runtimes reload
module initialization as well as handler code. `/tmp` state is lost when a container is replaced.
There is no watcher or debounce delay. Changes during an invocation take effect on the
following invocation. Edits that preserve all tracked metadata cannot be detected.

For `provided.al2023`/Go, Java, .NET and other compiled code, rebuild the Linux binaries,
classes or assemblies in the directory yourself. TinyStack runs the artifacts present there;
it does not compile source. A custom runtime needs an executable `bootstrap`.

The path survives a TinyStack restart in persistent mode. Keep the directory available at
the same path. Published versions and aliases also retain a **live reference**, so publishing
does not freeze its contents. `CodeSize` is zero and `CodeSha256` is the base64 SHA-256 of the
clean absolute path; these metadata values do not change with file edits. `GetFunction`
returns `Code.RepositoryType = "hot-reload"` and omits `Code.Location`, because there is no
ZIP download URL.

Image packages and hot reload layer publishing are rejected explicitly. Symlinks and special
files inside the directory are rejected at invocation. In-process runtimes can store and
manage these functions but reject directory invocations explicitly; execution requires Docker.
