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
    acm            = var.endpoint
    autoscaling    = var.endpoint
    apigateway     = var.endpoint
    apigatewayv2   = var.endpoint
    appsync        = var.endpoint
    cloudformation = var.endpoint
    cloudwatch     = var.endpoint
    cognitoidp     = var.endpoint
    dynamodb       = var.endpoint
    ec2            = var.endpoint
    ecr            = var.endpoint
    ecs            = var.endpoint
    eks            = var.endpoint
    elbv2          = var.endpoint
    eventbridge    = var.endpoint
    firehose       = var.endpoint
    iam            = var.endpoint
    kinesis        = var.endpoint
    kms            = var.endpoint
    lambda         = var.endpoint
    logs           = var.endpoint
    route53        = var.endpoint
    s3             = var.endpoint
    scheduler      = var.endpoint
    secretsmanager = var.endpoint
    sfn            = var.endpoint
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

data "aws_availability_zones" "available" {
  state = "available"
}

data "aws_subnets" "default" {
  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

resource "aws_ec2_tag" "subnet" {
  resource_id = data.aws_subnets.default.ids[0]
  key         = "team"
  value       = var.team
}

resource "aws_security_group" "web" {
  name        = "web"
  description = "Web servers"
  tags        = local.tags

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_vpc_security_group_ingress_rule" "https" {
  security_group_id = aws_security_group.web.id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_key_pair" "deploy" {
  key_name   = "deploy"
  public_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAiGFuOTUakKkbrTkNONMQ+7aXDyAdX1ytmLYkAxeh1m"
  tags       = local.tags
}

# A VPC with public and private subnets, the layout of the terraform-aws-modules/vpc module.
resource "aws_vpc" "app" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  tags                 = local.tags
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.app.id
  cidr_block              = "10.0.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true
}

resource "aws_subnet" "private" {
  vpc_id            = aws_vpc.app.id
  cidr_block        = "10.0.2.0/24"
  availability_zone = data.aws_availability_zones.available.names[1]
}

resource "aws_internet_gateway" "app" {
  vpc_id = aws_vpc.app.id
  tags   = local.tags
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.app.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.app.id
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_eip" "nat" {
  domain     = "vpc"
  depends_on = [aws_internet_gateway.app]
}

resource "aws_nat_gateway" "app" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public.id
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.app.id
}

resource "aws_route" "private_internet" {
  route_table_id         = aws_route_table.private.id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.app.id
}

resource "aws_route_table_association" "private" {
  subnet_id      = aws_subnet.private.id
  route_table_id = aws_route_table.private.id
}

resource "aws_network_acl" "private" {
  vpc_id     = aws_vpc.app.id
  subnet_ids = [aws_subnet.private.id]

  ingress {
    protocol   = "tcp"
    rule_no    = 100
    action     = "allow"
    cidr_block = aws_vpc.app.cidr_block
    from_port  = 443
    to_port    = 443
  }

  egress {
    protocol   = "-1"
    rule_no    = 100
    action     = "allow"
    cidr_block = "0.0.0.0/0"
    from_port  = 0
    to_port    = 0
  }
}

resource "aws_vpc_endpoint" "s3" {
  vpc_id          = aws_vpc.app.id
  service_name    = "com.amazonaws.us-east-1.s3"
  route_table_ids = [aws_route_table.private.id]
}

resource "aws_vpc_endpoint" "sqs" {
  vpc_id              = aws_vpc.app.id
  service_name        = "com.amazonaws.us-east-1.sqs"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = [aws_subnet.private.id]
  private_dns_enabled = true
}

data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-x86_64"]
  }
}

data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

# Systems Manager's public parameter names the image DescribeImages finds.
output "image_matches" {
  value = data.aws_ami.al2023.id == nonsensitive(data.aws_ssm_parameter.al2023.value)
}

output "zones" {
  value = length(data.aws_availability_zones.available.names)
}

resource "aws_cognito_user_pool" "app" {
  name                     = "app-users"
  username_attributes      = ["email"]
  auto_verified_attributes = ["email"]
  tags                     = local.tags

  password_policy {
    minimum_length    = 10
    require_lowercase = true
    require_numbers   = true
    require_symbols   = false
    require_uppercase = true
  }

  schema {
    name                = "team"
    attribute_data_type = "String"
    mutable             = true

    string_attribute_constraints {
      min_length = 1
      max_length = 64
    }
  }
}

resource "aws_cognito_user_pool_client" "web" {
  name                = "web"
  user_pool_id        = aws_cognito_user_pool.app.id
  generate_secret     = true
  explicit_auth_flows = ["ALLOW_USER_PASSWORD_AUTH", "ALLOW_REFRESH_TOKEN_AUTH"]
}

resource "aws_cognito_user_group" "admins" {
  name         = "admins"
  user_pool_id = aws_cognito_user_pool.app.id
  description  = "Administrators of ${var.team}"
}

resource "aws_cognito_user" "ana" {
  user_pool_id   = aws_cognito_user_pool.app.id
  username       = "ana@example.com"
  message_action = "SUPPRESS"
  password       = "Correct-Horse-9"

  attributes = {
    email          = "ana@example.com"
    email_verified = true
    team           = var.team
  }
}

resource "aws_cognito_user_in_group" "ana" {
  user_pool_id = aws_cognito_user_pool.app.id
  group_name   = aws_cognito_user_group.admins.name
  username     = aws_cognito_user.ana.username
}

resource "aws_ecr_repository" "app" {
  name                 = "team/app"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = true
  tags                 = local.tags

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "app" {
  repository = aws_ecr_repository.app.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep ten ${var.team} images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 10 }
      action       = { type = "expire" }
    }]
  })
}

resource "aws_ecr_repository_policy" "app" {
  repository = aws_ecr_repository.app.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "pull"
      Effect    = "Allow"
      Principal = { AWS = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root" }
      Action    = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
    }]
  })
}

resource "aws_ecs_cluster" "apps" {
  name = "apps"
  tags = local.tags

  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

resource "aws_ecs_cluster_capacity_providers" "apps" {
  cluster_name       = aws_ecs_cluster.apps.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
    base              = 1
  }
}

# Each team's image runs as a new revision.
resource "aws_ecs_task_definition" "web" {
  family                   = "web"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = "256"
  memory                   = "512"
  tags                     = local.tags

  container_definitions = jsonencode([{
    name         = "web"
    image        = "${aws_ecr_repository.app.repository_url}:${var.team}"
    essential    = true
    portMappings = [{ containerPort = 8080, protocol = "tcp" }]
    environment  = [{ name = "TEAM", value = var.team }]
  }])
}

resource "aws_ecs_service" "web" {
  name            = "web"
  cluster         = aws_ecs_cluster.apps.id
  task_definition = aws_ecs_task_definition.web.arn
  desired_count   = 2
  launch_type     = "FARGATE"
  tags            = local.tags

  network_configuration {
    subnets         = [data.aws_subnets.default.ids[0]]
    security_groups = [aws_security_group.web.id]
  }
}

resource "aws_iam_role" "eks" {
  name = "eks"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = ["eks.amazonaws.com", "ec2.amazonaws.com", "eks-pods.amazonaws.com"] }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
    }]
  })
}

resource "aws_iam_instance_profile" "nodes" {
  name = "nodes"
  path = "/eks/"
  role = aws_iam_role.eks.name
  tags = local.tags
}

resource "aws_eks_cluster" "apps" {
  name     = "apps"
  role_arn = aws_iam_role.eks.arn
  tags     = local.tags

  vpc_config {
    subnet_ids = data.aws_subnets.default.ids
  }

  access_config {
    authentication_mode = "API"
  }

  enabled_cluster_log_types = ["api", "audit"]
}

resource "aws_eks_node_group" "workers" {
  cluster_name    = aws_eks_cluster.apps.name
  node_group_name = "workers"
  node_role_arn   = aws_iam_role.eks.arn
  subnet_ids      = data.aws_subnets.default.ids
  labels          = { team = var.team }
  tags            = local.tags

  scaling_config {
    desired_size = 2
    min_size     = 1
    max_size     = 3
  }
}

resource "aws_eks_fargate_profile" "jobs" {
  cluster_name           = aws_eks_cluster.apps.name
  fargate_profile_name   = "jobs"
  pod_execution_role_arn = aws_iam_role.eks.arn
  subnet_ids             = data.aws_subnets.default.ids
  tags                   = local.tags

  selector {
    namespace = "jobs"
  }
}

resource "aws_eks_addon" "coredns" {
  cluster_name = aws_eks_cluster.apps.name
  addon_name   = "coredns"
  tags         = local.tags
}

resource "aws_eks_access_entry" "ops" {
  cluster_name      = aws_eks_cluster.apps.name
  principal_arn     = aws_iam_role.lambda.arn
  kubernetes_groups = ["ops"]
  tags              = local.tags
}

resource "aws_eks_access_policy_association" "ops" {
  cluster_name  = aws_eks_cluster.apps.name
  principal_arn = aws_eks_access_entry.ops.principal_arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"

  access_scope {
    type       = "namespace"
    namespaces = [var.team]
  }
}

resource "aws_eks_pod_identity_association" "app" {
  cluster_name    = aws_eks_cluster.apps.name
  namespace       = "default"
  service_account = "app"
  role_arn        = aws_iam_role.eks.arn
  tags            = local.tags
}

resource "aws_eks_identity_provider_config" "google" {
  cluster_name = aws_eks_cluster.apps.name
  tags         = local.tags

  oidc {
    identity_provider_config_name = "google"
    issuer_url                    = "https://accounts.google.com"
    client_id                     = "tinystack"
    groups_claim                  = "groups"
  }
}

data "aws_ecr_authorization_token" "registry" {}

output "registry_user" {
  value = data.aws_ecr_authorization_token.registry.user_name
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

resource "aws_sns_topic_subscription" "oncall" {
  topic_arn     = aws_sns_topic.alerts.arn
  protocol      = "email"
  endpoint      = "oncall@example.com"
  filter_policy = jsonencode({ team = [var.team] })
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

resource "aws_sqs_queue" "orders" {
  name                        = "orders.fifo"
  fifo_queue                  = true
  content_based_deduplication = true
  deduplication_scope         = "messageGroup"
  fifo_throughput_limit       = "perMessageGroupId"
  tags                        = local.tags
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

resource "aws_appsync_graphql_api" "notes" {
  name                = "notes-${var.team}"
  authentication_type = "API_KEY"
  tags                = local.tags

  additional_authentication_provider {
    authentication_type = "AWS_IAM"
  }

  schema = <<-EOT
    type Note @aws_api_key @aws_iam {
      id: ID!
      team: String
    }

    type Query {
      note(id: ID!): Note @aws_api_key @aws_iam
      notes: [Note]
    }

    schema {
      query: Query
    }
  EOT
}

resource "aws_appsync_api_key" "notes" {
  api_id      = aws_appsync_graphql_api.notes.id
  description = "Key for ${var.team}"
}

resource "aws_appsync_datasource" "echo" {
  api_id           = aws_appsync_graphql_api.notes.id
  name             = "echo"
  type             = "AWS_LAMBDA"
  service_role_arn = aws_iam_role.lambda.arn

  lambda_config {
    function_arn = aws_lambda_function.echo.arn
  }
}

resource "aws_appsync_resolver" "note" {
  api_id      = aws_appsync_graphql_api.notes.id
  type        = "Query"
  field       = "note"
  data_source = aws_appsync_datasource.echo.name
}

resource "aws_appsync_function" "load" {
  api_id      = aws_appsync_graphql_api.notes.id
  data_source = aws_appsync_datasource.echo.name
  name        = "load_${var.team}"

  runtime {
    name            = "APPSYNC_JS"
    runtime_version = "1.0.0"
  }

  code = <<-EOT
    export function request(ctx) {
      return { operation: "Invoke", payload: ctx.args };
    }

    export function response(ctx) {
      return ctx.result;
    }
  EOT
}

resource "aws_appsync_resolver" "notes" {
  api_id = aws_appsync_graphql_api.notes.id
  type   = "Query"
  field  = "notes"
  kind   = "PIPELINE"

  runtime {
    name            = "APPSYNC_JS"
    runtime_version = "1.0.0"
  }

  pipeline_config {
    functions = [aws_appsync_function.load.function_id]
  }

  code = <<-EOT
    export function request(ctx) {
      return {};
    }

    export function response(ctx) {
      return ctx.prev.result;
    }
  EOT
}

resource "aws_apigatewayv2_api" "echo" {
  name          = "echo"
  protocol_type = "HTTP"
  description   = "Echo for ${var.team}"
  tags          = local.tags

  cors_configuration {
    allow_origins = ["https://${var.team}.example"]
    allow_methods = ["GET", "POST"]
    max_age       = 300
  }
}

resource "aws_apigatewayv2_integration" "echo" {
  api_id                 = aws_apigatewayv2_api.echo.id
  integration_type       = "AWS_PROXY"
  integration_uri        = aws_lambda_function.echo.invoke_arn
  payload_format_version = "2.0"
  timeout_milliseconds   = 10000
}

resource "aws_apigatewayv2_authorizer" "users" {
  api_id           = aws_apigatewayv2_api.echo.id
  name             = "users"
  authorizer_type  = "JWT"
  identity_sources = ["$request.header.Authorization"]

  jwt_configuration {
    issuer   = "https://${aws_cognito_user_pool.app.endpoint}"
    audience = [aws_cognito_user_pool_client.web.id]
  }
}

resource "aws_apigatewayv2_route" "hello" {
  api_id    = aws_apigatewayv2_api.echo.id
  route_key = "GET /hello/{name}"
  target    = "integrations/${aws_apigatewayv2_integration.echo.id}"
}

resource "aws_apigatewayv2_route" "me" {
  api_id             = aws_apigatewayv2_api.echo.id
  route_key          = "GET /me"
  target             = "integrations/${aws_apigatewayv2_integration.echo.id}"
  authorization_type = "JWT"
  authorizer_id      = aws_apigatewayv2_authorizer.users.id
}

resource "aws_cloudwatch_log_group" "api" {
  name = "/aws/apigateway/echo"
}

resource "aws_apigatewayv2_stage" "default" {
  api_id          = aws_apigatewayv2_api.echo.id
  name            = "$default"
  auto_deploy     = true
  stage_variables = { team = var.team }
  tags            = local.tags

  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.api.arn
    format          = "$context.requestId $context.routeKey $context.status"
  }

  default_route_settings {
    throttling_burst_limit = 100
    throttling_rate_limit  = 50
  }
}

resource "aws_api_gateway_rest_api" "shop" {
  name        = "shop"
  description = "Shop for ${var.team}"
  tags        = local.tags

  endpoint_configuration {
    types = ["REGIONAL"]
  }
}

resource "aws_api_gateway_resource" "items" {
  rest_api_id = aws_api_gateway_rest_api.shop.id
  parent_id   = aws_api_gateway_rest_api.shop.root_resource_id
  path_part   = "items"
}

resource "aws_api_gateway_resource" "item" {
  rest_api_id = aws_api_gateway_rest_api.shop.id
  parent_id   = aws_api_gateway_resource.items.id
  path_part   = "{id}"
}

resource "aws_api_gateway_authorizer" "users" {
  rest_api_id   = aws_api_gateway_rest_api.shop.id
  name          = "users"
  type          = "COGNITO_USER_POOLS"
  provider_arns = [aws_cognito_user_pool.app.arn]
}

resource "aws_api_gateway_method" "get_item" {
  rest_api_id        = aws_api_gateway_rest_api.shop.id
  resource_id        = aws_api_gateway_resource.item.id
  http_method        = "GET"
  authorization      = "COGNITO_USER_POOLS"
  authorizer_id      = aws_api_gateway_authorizer.users.id
  request_parameters = { "method.request.path.id" = true }
}

resource "aws_api_gateway_integration" "get_item" {
  rest_api_id             = aws_api_gateway_rest_api.shop.id
  resource_id             = aws_api_gateway_resource.item.id
  http_method             = aws_api_gateway_method.get_item.http_method
  type                    = "AWS_PROXY"
  integration_http_method = "POST"
  uri                     = aws_lambda_function.echo.invoke_arn
}

resource "aws_api_gateway_method" "options_items" {
  rest_api_id   = aws_api_gateway_rest_api.shop.id
  resource_id   = aws_api_gateway_resource.items.id
  http_method   = "OPTIONS"
  authorization = "NONE"
}

resource "aws_api_gateway_integration" "options_items" {
  rest_api_id       = aws_api_gateway_rest_api.shop.id
  resource_id       = aws_api_gateway_resource.items.id
  http_method       = aws_api_gateway_method.options_items.http_method
  type              = "MOCK"
  request_templates = { "application/json" = jsonencode({ statusCode = 200 }) }
}

resource "aws_api_gateway_method_response" "options_items" {
  rest_api_id         = aws_api_gateway_rest_api.shop.id
  resource_id         = aws_api_gateway_resource.items.id
  http_method         = aws_api_gateway_method.options_items.http_method
  status_code         = "200"
  response_parameters = { "method.response.header.Access-Control-Allow-Origin" = true }
}

resource "aws_api_gateway_integration_response" "options_items" {
  rest_api_id         = aws_api_gateway_rest_api.shop.id
  resource_id         = aws_api_gateway_resource.items.id
  http_method         = aws_api_gateway_method.options_items.http_method
  status_code         = aws_api_gateway_method_response.options_items.status_code
  response_parameters = { "method.response.header.Access-Control-Allow-Origin" = "'https://${var.team}.example'" }
  depends_on          = [aws_api_gateway_integration.options_items]
}

resource "aws_api_gateway_deployment" "shop" {
  rest_api_id = aws_api_gateway_rest_api.shop.id
  description = "Deployed for ${var.team}"

  triggers = {
    redeploy = sha1(jsonencode([
      aws_api_gateway_integration.get_item.id, aws_api_gateway_integration_response.options_items.id,
      aws_api_gateway_integration_response.options_items.response_parameters,
    ]))
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_api_gateway_stage" "live" {
  rest_api_id   = aws_api_gateway_rest_api.shop.id
  deployment_id = aws_api_gateway_deployment.shop.id
  stage_name    = "live"
  variables     = { team = var.team }
  tags          = local.tags

  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.api.arn
    format          = "$context.requestId $context.resourcePath $context.status"
  }
}

resource "aws_api_gateway_method_settings" "all" {
  rest_api_id = aws_api_gateway_rest_api.shop.id
  stage_name  = aws_api_gateway_stage.live.stage_name
  method_path = "*/*"

  settings {
    metrics_enabled        = true
    throttling_burst_limit = 50
  }
}

resource "aws_lambda_permission" "shop" {
  statement_id  = "shop"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.echo.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_api_gateway_rest_api.shop.execution_arn}/*/*"
}

resource "aws_lambda_permission" "api" {
  statement_id  = "api"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.echo.function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.echo.execution_arn}/*/*"
}

resource "aws_sfn_activity" "review" {
  name = "review"
  tags = local.tags
}

resource "aws_cloudwatch_log_group" "workflow" {
  name = "/aws/states/orders"
}

resource "aws_sfn_state_machine" "orders" {
  name     = "orders"
  role_arn = aws_iam_role.lambda.arn
  tags     = local.tags

  definition = jsonencode({
    Comment = "Orders for ${var.team}"
    StartAt = "Echo"
    States = {
      Echo = {
        Type       = "Task"
        Resource   = "arn:aws:states:::lambda:invoke"
        Parameters = { FunctionName = aws_lambda_function.echo.arn, "Payload.$" = "$" }
        Retry      = [{ ErrorEquals = ["States.TaskFailed"], MaxAttempts = 2 }]
        Next       = "Review"
      }
      Review = { Type = "Task", Resource = aws_sfn_activity.review.id, End = true }
    }
  })

  logging_configuration {
    log_destination        = "${aws_cloudwatch_log_group.workflow.arn}:*"
    include_execution_data = true
    level                  = "ERROR"
  }
}

resource "aws_kinesis_stream" "clicks" {
  name             = "clicks"
  shard_count      = 2
  retention_period = 48
  encryption_type  = "KMS"
  kms_key_id       = "alias/aws/kinesis"
  tags             = local.tags

  shard_level_metrics = ["IncomingBytes"]
}

resource "aws_kinesis_firehose_delivery_stream" "clicks" {
  name        = "clicks"
  destination = "extended_s3"
  tags        = local.tags

  kinesis_source_configuration {
    kinesis_stream_arn = aws_kinesis_stream.clicks.arn
    role_arn           = aws_iam_role.lambda.arn
  }

  extended_s3_configuration {
    role_arn            = aws_iam_role.lambda.arn
    bucket_arn          = aws_s3_bucket.assets.arn
    prefix              = "clicks/${var.team}/"
    error_output_prefix = "errors/"
    buffering_interval  = 60
    buffering_size      = 5
    compression_format  = "GZIP"
  }
}

resource "aws_lambda_event_source_mapping" "clicks" {
  event_source_arn  = aws_kinesis_stream.clicks.arn
  function_name     = aws_lambda_function.echo.arn
  starting_position = "LATEST"
  batch_size        = 50
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

  retry_policy {
    maximum_retry_attempts       = 3
    maximum_event_age_in_seconds = 600
  }

  dead_letter_config {
    arn = aws_sqs_queue.dead.arn
  }
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

resource "aws_iam_user_policy" "deployer" {
  name = "deploy"
  user = aws_iam_user.deployer.name
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["lambda:UpdateFunctionCode"], Resource = "*" }]
  })
}

resource "aws_iam_user_policy_attachment" "deployer" {
  user       = aws_iam_user.deployer.name
  policy_arn = aws_iam_policy.assets.arn
}

resource "aws_iam_group" "deployers" {
  name = "deployers"
  path = "/ci/"
}

resource "aws_iam_user_group_membership" "deployer" {
  user   = aws_iam_user.deployer.name
  groups = [aws_iam_group.deployers.name]
}

resource "aws_iam_group_policy" "deployers" {
  name  = "logs"
  group = aws_iam_group.deployers.name
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["logs:GetLogEvents"], Resource = "*" }]
  })
}

resource "aws_iam_group_policy_attachment" "deployers" {
  group      = aws_iam_group.deployers.name
  policy_arn = aws_iam_policy.assets.arn
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

resource "aws_kms_key" "mac" {
  description              = "Webhook signatures"
  key_usage                = "GENERATE_VERIFY_MAC"
  customer_master_key_spec = "HMAC_256"
  deletion_window_in_days  = 7
  tags                     = local.tags
}

resource "aws_kms_alias" "data" {
  name          = "alias/data"
  target_key_id = aws_kms_key.data.key_id
}

resource "aws_cloudwatch_metric_alarm" "errors" {
  alarm_name          = "errors"
  alarm_description   = "Errors for ${var.team}"
  namespace           = "App"
  metric_name         = "Errors"
  dimensions          = { Service = "api" }
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 2
  datapoints_to_alarm = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  tags                = local.tags
}

resource "aws_cloudwatch_metric_alarm" "error_rate" {
  alarm_name          = "error-rate"
  evaluation_periods  = 1
  threshold           = 5
  comparison_operator = "GreaterThanThreshold"

  metric_query {
    id          = "rate"
    expression  = "errors / requests * 100"
    label       = "Error rate"
    return_data = true
  }

  metric_query {
    id = "errors"
    metric {
      namespace   = "App"
      metric_name = "Errors"
      period      = 60
      stat        = "Sum"
    }
  }

  metric_query {
    id = "requests"
    metric {
      namespace   = "App"
      metric_name = "Requests"
      period      = 60
      stat        = "Sum"
    }
  }
}

resource "aws_cloudwatch_composite_alarm" "service" {
  alarm_name        = "service"
  alarm_description = "Service health for ${var.team}"
  alarm_rule        = "ALARM(${aws_cloudwatch_metric_alarm.errors.alarm_name}) OR ALARM(${aws_cloudwatch_metric_alarm.error_rate.alarm_name})"
  alarm_actions     = [aws_sns_topic.alerts.arn]
  tags              = local.tags
}

resource "aws_cloudwatch_dashboard" "main" {
  dashboard_name = "main"
  dashboard_body = jsonencode({
    widgets = [{
      type       = "text", x = 0, y = 0, width = 6, height = 3
      properties = { markdown = "Owned by ${var.team}" }
    }]
  })
}

resource "aws_acm_certificate" "site" {
  domain_name               = "example.com"
  subject_alternative_names = ["*.example.com"]
  validation_method         = "DNS"
  key_algorithm             = "EC_prime256v1"
  tags                      = local.tags

  options {
    certificate_transparency_logging_preference = "ENABLED"
  }
}

resource "aws_acm_certificate_validation" "site" {
  certificate_arn         = aws_acm_certificate.site.arn
  validation_record_fqdns = [for r in aws_route53_record.validation : r.fqdn]
}

resource "aws_route53_zone" "site" {
  name    = "example.com"
  comment = "Site for ${var.team}"
  tags    = local.tags
}

resource "aws_route53_record" "validation" {
  for_each = {
    for o in aws_acm_certificate.site.domain_validation_options : o.domain_name => o
  }

  allow_overwrite = true
  zone_id         = aws_route53_zone.site.zone_id
  name            = each.value.resource_record_name
  type            = each.value.resource_record_type
  ttl             = 60
  records         = [each.value.resource_record_value]
}

resource "aws_route53_health_check" "www" {
  fqdn              = "www.example.com"
  port              = 443
  type              = "HTTPS"
  resource_path     = "/health"
  failure_threshold = 3
  request_interval  = 30
  tags              = local.tags
}

resource "aws_route53_record" "www" {
  zone_id         = aws_route53_zone.site.zone_id
  name            = "www.example.com"
  type            = "A"
  ttl             = 300
  records         = ["10.0.0.1"]
  set_identifier  = "primary"
  health_check_id = aws_route53_health_check.www.id

  failover_routing_policy {
    type = "PRIMARY"
  }
}

resource "aws_route53_record" "team" {
  zone_id = aws_route53_zone.site.zone_id
  name    = "team.example.com"
  type    = "TXT"
  ttl     = 300
  records = [var.team]
}

resource "aws_scheduler_schedule_group" "jobs" {
  name = "jobs"
  tags = local.tags
}

resource "aws_scheduler_schedule" "nightly" {
  name                         = "nightly"
  group_name                   = aws_scheduler_schedule_group.jobs.name
  schedule_expression          = "cron(0 2 * * ? *)"
  schedule_expression_timezone = "Europe/Berlin"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_sqs_queue.dead.arn
    role_arn = aws_iam_role.lambda.arn
    input    = jsonencode({ team = var.team })
  }
}

resource "aws_launch_template" "web" {
  name                   = "web"
  image_id               = data.aws_ami.al2023.id
  instance_type          = "t3.micro"
  update_default_version = true
  user_data              = base64encode("#!/bin/sh\necho ${var.team}\n")
  tags                   = local.tags

  tag_specifications {
    resource_type = "instance"
    tags          = local.tags
  }
}

resource "aws_autoscaling_group" "web" {
  name               = "web"
  min_size           = 1
  max_size           = 3
  desired_capacity   = 2
  availability_zones = ["us-east-1a", "us-east-1b"]

  launch_template {
    id      = aws_launch_template.web.id
    version = "$Latest"
  }

  tag {
    key                 = "team"
    value               = var.team
    propagate_at_launch = true
  }
}

resource "aws_autoscaling_policy" "up" {
  name                   = "up"
  autoscaling_group_name = aws_autoscaling_group.web.name
  adjustment_type        = "ChangeInCapacity"
  scaling_adjustment     = 1
  cooldown               = 120
}

resource "aws_autoscaling_policy" "cpu" {
  name                   = "cpu"
  autoscaling_group_name = aws_autoscaling_group.web.name
  policy_type            = "TargetTrackingScaling"

  target_tracking_configuration {
    target_value = var.team == "platform" ? 60 : 50

    predefined_metric_specification {
      predefined_metric_type = "ASGAverageCPUUtilization"
    }
  }
}

resource "aws_autoscaling_schedule" "night" {
  scheduled_action_name  = "night"
  autoscaling_group_name = aws_autoscaling_group.web.name
  recurrence             = "0 22 * * *"
  time_zone              = "Europe/Berlin"
  min_size               = 1
  max_size               = 3
  desired_capacity       = 1
}

resource "aws_lb" "web" {
  name               = "web"
  load_balancer_type = "application"
  subnets            = slice(data.aws_subnets.default.ids, 0, 2)
  tags               = local.tags
}

resource "aws_lb_target_group" "web" {
  name        = "web"
  port        = 80
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = aws_lb.web.vpc_id
  tags        = local.tags

  health_check {
    path = "/health"
  }
}

resource "aws_lb_target_group_attachment" "web" {
  target_group_arn = aws_lb_target_group.web.arn
  target_id        = "172.31.0.10"
  port             = 80
}

resource "aws_lb_listener" "web" {
  load_balancer_arn = aws_lb.web.arn
  port              = 80
  protocol          = "HTTP"
  tags              = local.tags

  default_action {
    type = "fixed-response"

    fixed_response {
      content_type = "text/plain"
      message_body = var.team
      status_code  = "404"
    }
  }
}

resource "aws_lb_listener_rule" "api" {
  listener_arn = aws_lb_listener.web.arn
  priority     = 10
  tags         = local.tags

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.web.arn
  }

  condition {
    path_pattern {
      values = ["/api/*"]
    }
  }
}

# A stack whose parameter and tags follow the team, so an update changes its
# queue's tags and its parameter's value in place.
resource "aws_cloudformation_stack" "app" {
  name       = "tinystack-app"
  parameters = { Team = var.team }
  tags       = local.tags

  template_body = jsonencode({
    Parameters = {
      Team = { Type = "String" }
    }
    Resources = {
      Queue = {
        Type       = "AWS::SQS::Queue"
        Properties = { Tags = [{ Key = "team", Value = { Ref = "Team" } }] }
      }
      Parameter = {
        Type       = "AWS::SSM::Parameter"
        Properties = { Type = "String", Value = { "Fn::Sub" = "team-$${Team}" } }
      }
    }
    Outputs = {
      QueueUrl  = { Value = { Ref = "Queue" } }
      Parameter = { Value = { "Fn::GetAtt" = ["Parameter", "Value"] } }
    }
  })
}

output "stack_parameter" {
  value = aws_cloudformation_stack.app.outputs["Parameter"]
}
