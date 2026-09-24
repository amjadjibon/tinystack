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
  access_key                  = "test"
  secret_key                  = "test"
  region                      = "us-east-1"
  skip_metadata_api_check = true

  endpoints {
    dynamodb    = var.endpoint
    eventbridge = var.endpoint
    iam         = var.endpoint
    lambda      = var.endpoint
    s3          = var.endpoint
    sns         = var.endpoint
    sqs         = var.endpoint
    sts         = var.endpoint
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
}

resource "aws_dynamodb_table" "users" {
  name         = "users"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "PK"

  attribute {
    name = "PK"
    type = "S"
  }

  tags = local.tags
}

resource "aws_s3_bucket" "assets" {
  bucket = "assets"
  tags   = local.tags
}

resource "aws_s3_object" "hello" {
  bucket       = aws_s3_bucket.assets.id
  key          = "hello.txt"
  content      = "hello"
  content_type = "text/plain"
}

resource "aws_lambda_function" "echo" {
  function_name    = "echo"
  role             = aws_iam_role.lambda.arn
  runtime          = "python3.12"
  handler          = "app.handler"
  filename         = var.function_zip
  source_code_hash = filebase64sha256(var.function_zip)
  tags             = local.tags
}

resource "aws_lambda_event_source_mapping" "jobs" {
  event_source_arn = aws_sqs_queue.jobs.arn
  function_name    = aws_lambda_function.echo.arn
  batch_size       = 5
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
