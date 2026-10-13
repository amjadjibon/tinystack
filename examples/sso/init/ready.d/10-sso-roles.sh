#!/bin/sh
set -eu

# The roles that console single sign-on maps groups to (see tinystack.yaml), created idempotently.
# TinyStack supplies this endpoint, the root credentials and the Region to ready hooks. DEVELOPMENT ONLY.
: "${AWS_ENDPOINT_URL:?Run this script as a TinyStack ready hook}"
export AWS_PAGER="" AWS_CLI_AUTO_PROMPT=off

if ! command -v aws >/dev/null 2>&1; then
  echo "The SSO example requires the AWS CLI (aws) on PATH." >&2
  exit 1
fi

# Account 000000000000 is the default account, so the supplied credentials are its root.
admin_aws() {
  aws --endpoint-url "$AWS_ENDPOINT_URL" --region "$AWS_DEFAULT_REGION" "$@"
}

# Account 111111111111 has its own root key.
team_b_aws() {
  AWS_ACCESS_KEY_ID=dev-root-b AWS_SECRET_ACCESS_KEY=dev-root-secret \
    aws --endpoint-url "$AWS_ENDPOINT_URL" --region "$AWS_DEFAULT_REGION" "$@"
}

# Console single sign-on does not evaluate trust policies: the group mapping in tinystack.yaml is the
# trust. The document only has to be valid.
trust='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}'

# Administrator: everything, in account 000000000000.
admin='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}'
# Developer: S3 and SQS read and write, nothing else (IAM, for one, is denied), in account 111111111111.
developer='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*","sqs:*"],"Resource":"*"}]}'

if ! admin_aws iam get-role --role-name Admin >/dev/null 2>&1; then
  admin_aws iam create-role --role-name Admin --assume-role-policy-document "$trust" \
    --max-session-duration 43200 >/dev/null
fi
admin_aws iam put-role-policy --role-name Admin --policy-name administrator --policy-document "$admin"

if ! team_b_aws iam get-role --role-name Developer >/dev/null 2>&1; then
  team_b_aws iam create-role --role-name Developer --assume-role-policy-document "$trust" \
    --max-session-duration 43200 >/dev/null
fi
team_b_aws iam put-role-policy --role-name Developer --policy-name s3-and-sqs --policy-document "$developer"

echo "SSO roles ready: Admin (000000000000) and Developer (111111111111)."
