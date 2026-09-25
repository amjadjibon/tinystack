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

resource "aws_sqs_queue" "jobs" {
  name                       = "jobs"
  visibility_timeout_seconds = 60
  kms_master_key_id          = aws_kms_alias.data.name
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dead.arn
    maxReceiveCount     = 3
  })
  tags = local.tags
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
  bucket       = aws_s3_bucket.assets.id
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
  event_source_arn = aws_sqs_queue.jobs.arn
  function_name    = aws_lambda_function.echo.arn
  batch_size       = 5
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
