#!/bin/sh
set -eu

# TinyStack supplies this endpoint, credentials and Region to ready hooks.
: "${AWS_ENDPOINT_URL:?Run this script as a TinyStack ready hook}"
export AWS_PAGER="" AWS_CLI_AUTO_PROMPT=off

if ! command -v aws >/dev/null 2>&1; then
  echo "The development seed requires the AWS CLI (aws) on PATH." >&2
  exit 1
fi

local_aws() {
  aws --endpoint-url "$AWS_ENDPOINT_URL" --region "$AWS_DEFAULT_REGION" "$@"
}

if ! local_aws iam get-user --user-name demo-user >/dev/null 2>&1; then
  local_aws iam create-user --user-name demo-user >/dev/null
fi

if ! local_aws iam get-group --group-name developers >/dev/null 2>&1; then
  local_aws iam create-group --group-name developers >/dev/null
fi
local_aws iam add-user-to-group --user-name demo-user --group-name developers

if ! local_aws s3api head-bucket --bucket tinystack-demo >/dev/null 2>&1; then
  if [ "$AWS_DEFAULT_REGION" = us-east-1 ]; then
    local_aws s3api create-bucket --bucket tinystack-demo >/dev/null
  else
    local_aws s3api create-bucket --bucket tinystack-demo \
      --create-bucket-configuration "LocationConstraint=$AWS_DEFAULT_REGION" >/dev/null
  fi
fi

if ! local_aws sqs get-queue-url --queue-name demo-jobs >/dev/null 2>&1; then
  local_aws sqs create-queue --queue-name demo-jobs >/dev/null
fi

echo "Development seed ready: demo-user in developers, tinystack-demo bucket, demo-jobs queue."
