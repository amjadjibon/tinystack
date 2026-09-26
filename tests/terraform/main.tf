# One resource per TinyStack service, applied by tests/terraform/terraform.py.

terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}

variable "endpoint" { type = string }
variable "function_zip" { type = string }
variable "team" { type = string }

provider "aws" {
  access_key              = "test"
  secret_key              = "test"
  region                  = "us-east-1"
  skip_metadata_api_check = true

  endpoints {
    dynamodb       = var.endpoint
    eventbridge    = var.endpoint
    iam            = var.endpoint
    kms            = var.endpoint
    lambda         = var.endpoint
    logs           = var.endpoint
    s3             = var.endpoint
    secretsmanager = var.endpoint
    sns            = var.endpoint
    sqs            = var.endpoint
    ssm            = var.endpoint
    sts            = var.endpoint
  }
}

# The provider validates its credentials and reads the account ID with STS.
data "aws_caller_identity" "current" {}

output "account_id" {
  value = data.aws_caller_identity.current.account_id
}

locals {
  tags = { team = var.team }
}

resource "aws_sqs_queue" "dead" {
  name = "jobs-dead"
}

# Only the jobs queue may use jobs-dead as its dead-letter queue.
resource "aws_sqs_queue_redrive_allow_policy" "dead" {
  queue_url = aws_sqs_queue.dead.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = ["arn:aws:sqs:us-east-1:${data.aws_caller_identity.current.account_id}:jobs"]
  })
}

resource "aws_sqs_queue" "jobs" {
  name                       = "jobs"
  visibility_timeout_seconds = 60
  kms_master_key_id          = aws_kms_alias.data.name
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dead.arn
    maxReceiveCount     = 3
  })
  tags = local.tags

  # The dead-letter queue must allow jobs before jobs can use it.
  depends_on = [aws_sqs_queue_redrive_allow_policy.dead]
}

resource "aws_sns_topic" "alerts" {
  name = "alerts"
  tags = local.tags
}

resource "aws_sns_topic_subscription" "jobs" {
  topic_arn            = aws_sns_topic.alerts.arn
  protocol             = "sqs"
  endpoint             = aws_sqs_queue.jobs.arn
  raw_message_delivery = true
  filter_policy        = jsonencode({ team = [var.team] })
}

resource "aws_dynamodb_table" "users" {
  name         = "users"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "PK"

  attribute {
    name = "PK"
    type = "S"
  }

  attribute {
    name = "email"
    type = "S"
  }

  global_secondary_index {
    name            = "by-email"
    hash_key        = "email"
    projection_type = "KEYS_ONLY"
  }

  stream_enabled   = true
  stream_view_type = "NEW_AND_OLD_IMAGES"

  ttl {
    attribute_name = "expires"
    enabled        = true
  }

  tags = local.tags
}

resource "aws_s3_bucket" "assets" {
  bucket = "assets"
  tags   = local.tags
}

# Objects are written after versioning is enabled, so each has a version ID;
# destroying them deletes every version, which lets the bucket be deleted.
resource "aws_s3_bucket_versioning" "assets" {
  bucket = aws_s3_bucket.assets.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_sqs_queue" "uploads" {
  name = "uploads"
}

resource "aws_s3_bucket_notification" "assets" {
  bucket      = aws_s3_bucket.assets.id
  eventbridge = true

  queue {
    queue_arn     = aws_sqs_queue.uploads.arn
    events        = ["s3:ObjectCreated:*"]
    filter_prefix = "uploads/"
  }
}

resource "aws_s3_object" "hello" {
  bucket       = aws_s3_bucket_versioning.assets.id
  key          = "hello.txt"
  content      = "hello"
  content_type = "text/plain"
  tags         = local.tags
}

# A copy keeps the source's tags unless it replaces them.
resource "aws_s3_object_copy" "hello" {
  bucket            = aws_s3_bucket.assets.id
  key               = "copies/hello.txt"
  source            = "${aws_s3_bucket.assets.id}/${aws_s3_object.hello.key}"
  tagging_directive = "REPLACE"
}

# The same package, deployed from the versioned assets bucket.
resource "aws_s3_object" "echo_zip" {
  bucket = aws_s3_bucket_versioning.assets.id
  key    = "lambda/echo.zip"
  source = var.function_zip
  etag   = filemd5(var.function_zip)
}

resource "aws_lambda_function" "from_s3" {
  function_name     = "echo-s3"
  role              = aws_iam_role.lambda.arn
  runtime           = "python3.12"
  handler           = "app.handler"
  s3_bucket         = aws_s3_object.echo_zip.bucket
  s3_key            = aws_s3_object.echo_zip.key
  s3_object_version = aws_s3_object.echo_zip.version_id
}

resource "aws_lambda_function" "echo" {
  function_name    = "echo"
  role             = aws_iam_role.lambda.arn
  runtime          = "python3.12"
  handler          = "app.handler"
  filename         = var.function_zip
  source_code_hash = filebase64sha256(var.function_zip)
  publish          = true
  tags             = local.tags

  environment {
    variables = { TEAM = var.team }
  }
}

# Each code change publishes a version, and the alias follows it.
resource "aws_lambda_alias" "live" {
  name             = "live"
  function_name    = aws_lambda_function.echo.function_name
  function_version = aws_lambda_function.echo.version
  description      = "Serves ${var.team}"
}

resource "aws_cloudwatch_log_group" "echo" {
  name              = "/aws/lambda/echo"
  retention_in_days = 7
  tags              = local.tags
}

resource "aws_lambda_event_source_mapping" "jobs" {
  event_source_arn                   = aws_sqs_queue.jobs.arn
  function_name                      = aws_lambda_function.echo.arn
  batch_size                         = 20
  maximum_batching_window_in_seconds = 1
  function_response_types            = ["ReportBatchItemFailures"]

  filter_criteria {
    filter {
      pattern = jsonencode({ body = { kind = ["job"] } })
    }
  }
}

resource "aws_lambda_event_source_mapping" "users" {
  event_source_arn  = aws_dynamodb_table.users.stream_arn
  function_name     = aws_lambda_function.echo.arn
  starting_position = "LATEST"
}

resource "aws_cloudwatch_event_rule" "orders" {
  name          = "orders"
  event_pattern = jsonencode({ source = ["shop"] })
  tags          = local.tags
}

resource "aws_cloudwatch_event_target" "orders" {
  rule      = aws_cloudwatch_event_rule.orders.name
  target_id = "jobs"
  arn       = aws_sqs_queue.jobs.arn
}

resource "aws_cloudwatch_event_rule" "nightly" {
  name                = "nightly"
  schedule_expression = "cron(0 3 * * ? *)"
}

resource "aws_cloudwatch_event_target" "nightly" {
  rule      = aws_cloudwatch_event_rule.nightly.name
  target_id = "jobs"
  arn       = aws_sqs_queue.jobs.arn
  input     = jsonencode({ job = "cleanup" })
}

resource "aws_cloudwatch_event_bus" "shop" {
  name        = "shop"
  description = "Events for ${var.team}"
  tags        = local.tags
}

resource "aws_cloudwatch_event_rule" "paid" {
  name           = "paid"
  event_bus_name = aws_cloudwatch_event_bus.shop.name
  event_pattern  = jsonencode({ source = ["shop"], "detail-type" = ["paid"] })
}

resource "aws_cloudwatch_event_target" "paid" {
  rule           = aws_cloudwatch_event_rule.paid.name
  event_bus_name = aws_cloudwatch_event_bus.shop.name
  target_id      = "jobs"
  arn            = aws_sqs_queue.jobs.arn

  input_transformer {
    input_paths    = { id = "$.detail.id" }
    input_template = "{\"order\": <id>}"
  }
}

resource "aws_iam_user" "deployer" {
  name = "deployer"
  path = "/ci/"
  tags = local.tags
}

resource "aws_iam_access_key" "deployer" {
  user = aws_iam_user.deployer.name
}

resource "aws_iam_role" "lambda" {
  name        = "lambda"
  description = "Runs ${var.team} functions"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy" "queue" {
  name = "queue"
  role = aws_iam_role.lambda.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["sqs:ReceiveMessage"], Resource = aws_sqs_queue.jobs.arn }]
  })
}

# The statement ID follows the team, so an update creates a new policy version.
resource "aws_iam_policy" "assets" {
  name = "read-assets"
  path = "/app/"
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Sid = var.team, Effect = "Allow", Action = ["s3:GetObject"], Resource = "*" }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "assets" {
  role       = aws_iam_role.lambda.name
  policy_arn = aws_iam_policy.assets.arn
}

resource "aws_iam_role_policy_attachment" "basic" {
  role       = aws_iam_role.lambda.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_ssm_parameter" "team" {
  name        = "/app/config/team"
  type        = "String"
  value       = var.team
  description = "Owning team"
  tags        = local.tags
}

resource "aws_ssm_parameter" "token" {
  name   = "/app/config/token"
  type   = "SecureString"
  value  = "token-${var.team}"
  key_id = aws_kms_key.data.arn
}

resource "aws_secretsmanager_secret" "db" {
  name                    = "app/db"
  description             = "Database for ${var.team}"
  recovery_window_in_days = 0
  kms_key_id              = aws_kms_key.data.arn
  tags                    = local.tags
}

resource "aws_secretsmanager_secret_version" "db" {
  secret_id     = aws_secretsmanager_secret.db.id
  secret_string = jsonencode({ user = var.team, password = "local" })
}

# Rotation is scheduled, not run now, so the test needs no Docker.
resource "aws_secretsmanager_secret_rotation" "db" {
  secret_id           = aws_secretsmanager_secret.db.id
  rotation_lambda_arn = aws_lambda_function.echo.arn
  rotate_immediately  = false

  rotation_rules {
    automatically_after_days = 30
  }
}

resource "aws_kms_key" "data" {
  description             = "Data key for ${var.team}"
  deletion_window_in_days = 7
  enable_key_rotation     = true
  tags                    = local.tags
}

resource "aws_kms_alias" "data" {
  name          = "alias/data"
  target_key_id = aws_kms_key.data.key_id
}
