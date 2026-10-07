# Terraform, CloudFormation, and SAM

## Use the Terraform wrapper

With TinyStack running and Terraform installed:

```sh
task build:tools
./bin/terraform-local -chdir=YOUR_TERRAFORM_DIRECTORY init
./bin/terraform-local -chdir=YOUR_TERRAFORM_DIRECTORY plan
```

Replace the directory placeholder with your project. The wrapper writes
`tinystack_providers_override.tf.json` into that working directory for the command, then removes it.
It configures supported provider endpoints, local credentials, path-style S3 and metadata-check settings.
Do not keep another file under that wrapper-owned name. Set `TINYSTACK_TF_NO_OVERRIDE` or `TF_NO_OVERRIDE`
to bypass creation when you manage provider settings yourself.

The endpoint map is defined in
[localcli/cli.go](https://github.com/amjadjibon/tinystack/blob/main/internal/localcli/cli.go).
Not every registered API has a provider endpoint key; not every AWS resource or property is implemented.

## Configure the provider directly

```hcl
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  access_key              = "test"
  secret_key              = "test"
  region                  = "us-east-1"
  skip_metadata_api_check = true
  s3_use_path_style       = true
  endpoints {
    sts = "http://127.0.0.1:4566"
    sqs = "http://127.0.0.1:4566"
    s3  = "http://127.0.0.1:4566"
  }
}

resource "aws_sqs_queue" "jobs" {
  name = "terraform-jobs"
}
```

Add endpoint overrides for every service your project uses. STS provides local account lookup.
[tests/terraform/main.tf](https://github.com/amjadjibon/tinystack/blob/main/tests/terraform/main.tf)
is the working fixture for supported resources/properties beyond this small example.

## Create a CloudFormation stack

Save this as `queue.yaml`:

```yaml
AWSTemplateFormatVersion: "2010-09-09"
Resources:
  Jobs:
    Type: AWS::SQS::Queue
    Properties:
      QueueName: cloudformation-jobs
Outputs:
  QueueUrl:
    Value: !Ref Jobs
```

```sh
./bin/aws-local cloudformation create-stack \
  --stack-name docs-queue --template-body file://queue.yaml
./bin/aws-local cloudformation wait stack-create-complete --stack-name docs-queue
./bin/aws-local cloudformation describe-stacks --stack-name docs-queue
```

The runner invokes resource-owning services in dependency order. Unknown resource types/properties/attributes
and unsupported template semantics fail validation. See the generated
[resource registry](cloudformation-resources.md) and [service limits](../services/cloudformation.md).
Clean up with `./bin/aws-local cloudformation delete-stack --stack-name docs-queue`.

## SAM and compatibility fixtures

`sam-local` supplies local endpoint and environment defaults to the installed SAM CLI. The CloudFormation
implementation expands a selected SAM subset; the wrapper does not implement missing transform features.
Use [tests/sam/template.yaml](https://github.com/amjadjibon/tinystack/blob/main/tests/sam/template.yaml) and
[its runner](https://github.com/amjadjibon/tinystack/blob/main/tests/sam/sam.py) for deploy/redeploy/delete examples.
Docker is needed to execute deployed functions.

```sh
task test:terraform
task test:sam
```

These opt-in runners build/start their own local servers. Terraform downloads its provider; SAM needs the SAM
and AWS CLIs. They manage fixture resources separately from your foreground instance.
