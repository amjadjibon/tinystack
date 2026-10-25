# One resource per TinyStack service, applied by tests/terraform/terraform.py.

terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 6.0" }
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
    bedrockagent         = var.endpoint
    bedrockagentcore     = var.endpoint
    transcribe           = var.endpoint
    dsql                 = var.endpoint
    mediaconnect         = var.endpoint
    shield               = var.endpoint
    mediaconvert         = var.endpoint
    datasync             = var.endpoint
    resourceexplorer2    = var.endpoint
    servicecatalog       = var.endpoint
    fis                  = var.endpoint
    accessanalyzer       = var.endpoint
    swf                  = var.endpoint
    globalaccelerator    = var.endpoint
    securityhub          = var.endpoint
    inspector2           = var.endpoint
    amplify              = var.endpoint
    signer               = var.endpoint
    waf                  = var.endpoint
    dms                  = var.endpoint
    kinesisanalyticsv2   = var.endpoint
    opensearchserverless = var.endpoint
    budgets              = var.endpoint
    acm                  = var.endpoint
    autoscaling          = var.endpoint
    apigateway           = var.endpoint
    apigatewayv2         = var.endpoint
    appsync              = var.endpoint
    cloudformation       = var.endpoint
    cloudwatch           = var.endpoint
    ce                   = var.endpoint
    cur                  = var.endpoint
    codeartifact         = var.endpoint
    cognitoidp           = var.endpoint
    dynamodb             = var.endpoint
    ec2                  = var.endpoint
    ecr                  = var.endpoint
    ecs                  = var.endpoint
    eks                  = var.endpoint
    elasticache          = var.endpoint
    emr                  = var.endpoint
    emrserverless        = var.endpoint
    elbv2                = var.endpoint
    eventbridge          = var.endpoint
    firehose             = var.endpoint
    iam                  = var.endpoint
    iot                  = var.endpoint
    kinesis              = var.endpoint
    kms                  = var.endpoint
    lambda               = var.endpoint
    lakeformation        = var.endpoint
    glue                 = var.endpoint
    logs                 = var.endpoint
    networkfirewall      = var.endpoint
    route53              = var.endpoint
    s3                   = var.endpoint
    scheduler            = var.endpoint
    secretsmanager       = var.endpoint
    servicequotas        = var.endpoint
    sfn                  = var.endpoint
    sns                  = var.endpoint
    sqs                  = var.endpoint
    ssm                  = var.endpoint
    sts                  = var.endpoint
    timestreamwrite      = var.endpoint

    # S3 Control prefixes the host with the account ID, and only a name under localhost resolves to loopback.
    s3control = replace(var.endpoint, "127.0.0.1", "localhost")
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
  assign_generated_ipv6_cidr_block = true
  cidr_block                       = "10.0.0.0/16"
  enable_dns_hostnames             = true
  tags                             = local.tags
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.app.id
  cidr_block              = "10.0.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true
}

resource "aws_subnet" "private" {
  ipv6_cidr_block                 = cidrsubnet(aws_vpc.app.ipv6_cidr_block, 8, 1)
  assign_ipv6_address_on_creation = true
  vpc_id                          = aws_vpc.app.id
  cidr_block                      = "10.0.2.0/24"
  availability_zone               = data.aws_availability_zones.available.names[1]
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

resource "aws_cognito_user_pool_domain" "app" {
  domain                = "local-tf-signin"
  user_pool_id          = aws_cognito_user_pool.app.id
  managed_login_version = 1
}

resource "aws_cognito_identity_provider" "company" {
  user_pool_id  = aws_cognito_user_pool.app.id
  provider_name = "CorporateOIDC"
  provider_type = "OIDC"
  provider_details = {
    oidc_issuer      = "https://idp.example.com"
    client_id        = "terraform-client"
    client_secret    = "terraform-provider-secret"
    authorize_scopes = "openid email profile"
  }
  attribute_mapping = {
    email = "email"
  }
}

resource "aws_cognito_user_pool_client" "web" {
  name                                 = "web"
  user_pool_id                         = aws_cognito_user_pool.app.id
  generate_secret                      = true
  explicit_auth_flows                  = ["ALLOW_USER_PASSWORD_AUTH", "ALLOW_REFRESH_TOKEN_AUTH"]
  allowed_oauth_flows_user_pool_client = true
  allowed_oauth_flows                  = ["code"]
  allowed_oauth_scopes                 = ["openid", "email", "profile"]
  callback_urls                        = ["http://localhost/callback"]
  logout_urls                          = ["http://localhost/signed-out"]
  default_redirect_uri                 = "http://localhost/callback"
  supported_identity_providers         = ["COGNITO", aws_cognito_identity_provider.company.provider_name]

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
    projection_type = "KEYS_ONLY"
    key_schema {
      attribute_name = "email"
      key_type       = "HASH"
    }
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

resource "aws_cloudwatch_metric_alarm" "billing" {
  alarm_name          = "billing"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "EstimatedCharges"
  namespace           = "AWS/Billing"
  period              = 21600
  statistic           = "Maximum"
  threshold           = 80
  alarm_actions       = [aws_sns_topic.alerts.arn]

  dimensions = {
    Currency = "USD"
  }

  tags = local.tags
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

resource "aws_elasticache_subnet_group" "cache" {
  name       = "cache"
  subnet_ids = [data.aws_subnets.default.ids[0]]
  tags       = local.tags
}

resource "aws_elasticache_parameter_group" "cache" {
  name   = "cache"
  family = "redis7"
  tags   = local.tags

  parameter {
    name  = "maxmemory-policy"
    value = "allkeys-lru"
  }
}

resource "aws_elasticache_cluster" "redis" {
  cluster_id           = "cache"
  engine               = "redis"
  node_type            = "cache.t3.micro"
  num_cache_nodes      = 1
  parameter_group_name = aws_elasticache_parameter_group.cache.name
  subnet_group_name    = aws_elasticache_subnet_group.cache.name
  tags                 = local.tags
}

resource "aws_elasticache_replication_group" "rep" {
  replication_group_id = "rep"
  description          = "Terraform test replication group"
  node_type            = "cache.t3.micro"
  num_cache_clusters   = 2
  parameter_group_name = aws_elasticache_parameter_group.cache.name
  subnet_group_name    = aws_elasticache_subnet_group.cache.name
  tags                 = local.tags
}

resource "aws_elasticache_serverless_cache" "serverless" {
  name       = "serverless"
  engine     = "redis"
  subnet_ids = slice(data.aws_subnets.default.ids, 0, 2)
  tags       = local.tags
}

resource "aws_iot_thing_type" "sensor" {
  name = "sensor"
  properties {
    description           = "Terraform test sensors"
    searchable_attributes = ["team"]
  }
  tags = local.tags
}

resource "aws_iot_thing" "probe" {
  name            = "probe"
  thing_type_name = aws_iot_thing_type.sensor.name
  attributes      = { team = var.team }
}

resource "aws_iot_thing_group" "fleet" {
  name = "fleet"
  properties {
    description = "Terraform test fleet"
    attribute_payload {
      attributes = { team = var.team }
    }
  }
  tags = local.tags
}

resource "aws_iot_thing_group_membership" "probe" {
  thing_name       = aws_iot_thing.probe.name
  thing_group_name = aws_iot_thing_group.fleet.name
}

resource "aws_iot_certificate" "probe" {
  active = true
}

resource "aws_iot_policy" "probe" {
  name = "probe"
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["iot:Connect", "iot:Publish"], Resource = ["*"] }]
  })
  tags = local.tags
}

resource "aws_iot_policy_attachment" "probe" {
  policy = aws_iot_policy.probe.name
  target = aws_iot_certificate.probe.arn
}

resource "aws_iot_thing_principal_attachment" "probe" {
  thing     = aws_iot_thing.probe.name
  principal = aws_iot_certificate.probe.arn
}

resource "aws_iot_topic_rule" "readings" {
  name        = "readings"
  description = "Terraform test readings of ${var.team}"
  enabled     = true
  sql         = "SELECT *, topic(2) AS team FROM 'readings/${var.team}'"
  sql_version = "2016-03-23"
  sqs {
    queue_url  = aws_sqs_queue.jobs.url
    role_arn   = aws_iam_role.lambda.arn
    use_base64 = false
  }
  error_action {
    republish {
      topic    = "errors/readings"
      role_arn = aws_iam_role.lambda.arn
    }
  }
  tags = local.tags
}

resource "aws_iot_authorizer" "devices" {
  name                    = "devices"
  authorizer_function_arn = aws_lambda_function.echo.arn
  signing_disabled        = false
  status                  = "ACTIVE"
  token_key_name          = "token-${var.team}"
  token_signing_public_keys = {
    key1 = <<-EOT
      -----BEGIN PUBLIC KEY-----
      MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAqDm/lcyGIBzORvggk2CG
      4wXeebMzsrnDkMwQ1LhXHR7WkVph0C8vg4i1cKTM9/9WTfLlDC3fpznVS4mivtCN
      hJoQz9/hQPxgFk5ldIRKUplyAk1HRKVMl0PBZWa7BB0+EF9lMc0S9j5ieTL39Xqt
      emFwkdB0oKQygv4X9JEmb5VuarAB0xNh/fMhrEfgaPMbFke7kj01e6TyR/mHnKiq
      hechCeyvTc6G1PYP2oLxYts9sleMVvRu51fEqjIN0S/66bf7q1KQ7lwkLNDEH3In
      OBZTkBc7eRHrpMlv4uQOqoB/Sn4E+bPv9dme/34yXQgfOh5WanJp4EGJd9RN14QH
      HwIDAQAB
      -----END PUBLIC KEY-----
    EOT
  }
  tags = local.tags
}

resource "aws_iot_role_alias" "devices" {
  alias               = "devices"
  role_arn            = aws_iam_role.lambda.arn
  credential_duration = 900
  tags                = local.tags
}

resource "aws_iot_provisioning_template" "fleet" {
  name                  = "fleet"
  description           = "Terraform test fleet of ${var.team}"
  provisioning_role_arn = aws_iam_role.lambda.arn
  enabled               = true
  template_body = jsonencode({
    Parameters = {
      SerialNumber                = { Type = "String" }
      "AWS::IoT::Certificate::Id" = { Type = "String" }
    }
    Resources = {
      thing = {
        Type       = "AWS::IoT::Thing"
        Properties = { ThingName = { Ref = "SerialNumber" } }
      }
      certificate = {
        Type       = "AWS::IoT::Certificate"
        Properties = { CertificateId = { Ref = "AWS::IoT::Certificate::Id" }, Status = "ACTIVE" }
      }
    }
  })
  tags = local.tags
}

resource "aws_iot_indexing_configuration" "fleet" {
  thing_indexing_configuration {
    thing_indexing_mode              = "REGISTRY_AND_SHADOW"
    thing_connectivity_indexing_mode = "STATUS"
  }
  thing_group_indexing_configuration {
    thing_group_indexing_mode = "ON"
  }
}

data "aws_iot_endpoint" "data" {
  endpoint_type = "iot:Data-ATS"
}

output "iot_endpoint" {
  value = data.aws_iot_endpoint.data.endpoint_address
}

# EC2 VPC Tier 1: real provider lifecycle across DHCP, IPv6, ENIs, logs, prefix lists, and peering.
resource "aws_vpc_dhcp_options" "app" {
  domain_name         = "tiny.internal"
  domain_name_servers = ["AmazonProvidedDNS"]
  tags                = local.tags
}

resource "aws_vpc_dhcp_options_association" "app" {
  vpc_id          = aws_vpc.app.id
  dhcp_options_id = aws_vpc_dhcp_options.app.id
}

resource "aws_egress_only_internet_gateway" "app" {
  vpc_id = aws_vpc.app.id
  tags   = local.tags
}

resource "aws_route" "ipv6_egress" {
  route_table_id              = aws_route_table.private.id
  destination_ipv6_cidr_block = "::/0"
  egress_only_gateway_id      = aws_egress_only_internet_gateway.app.id
}

resource "aws_vpc_ipv4_cidr_block_association" "app" {
  vpc_id     = aws_vpc.app.id
  cidr_block = "10.2.0.0/16"
}

resource "aws_network_interface" "app" {
  subnet_id         = aws_subnet.private.id
  description       = "Interface for ${var.team}"
  private_ips       = ["10.0.2.10", "10.0.2.11"]
  source_dest_check = false
  tags              = local.tags
}

resource "aws_flow_log" "app" {
  vpc_id          = aws_vpc.app.id
  traffic_type    = "ALL"
  log_destination = aws_cloudwatch_log_group.echo.arn
  iam_role_arn    = aws_iam_role.lambda.arn
  tags            = local.tags
}

resource "aws_ec2_managed_prefix_list" "app" {
  name           = "services"
  address_family = "IPv4"
  max_entries    = 3
  entry {
    cidr        = "10.3.0.0/24"
    description = "Service range"
  }
  tags = local.tags
}

resource "aws_vpc_security_group_ingress_rule" "prefix" {
  security_group_id = aws_security_group.web.id
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  prefix_list_id    = aws_ec2_managed_prefix_list.app.id
  description       = "Services for ${var.team}"
}

resource "aws_vpc" "peer" {
  cidr_block = "10.4.0.0/16"
  tags       = local.tags
}

resource "aws_vpc_peering_connection" "app" {
  # Associate the secondary CIDR before peering can enter pending-acceptance.
  depends_on = [aws_vpc_ipv4_cidr_block_association.app]

  vpc_id      = aws_vpc.app.id
  peer_vpc_id = aws_vpc.peer.id
  auto_accept = true
  tags        = local.tags
}

resource "aws_route" "peer" {
  route_table_id            = aws_route_table.private.id
  destination_cidr_block    = aws_vpc.peer.cidr_block
  vpc_peering_connection_id = aws_vpc_peering_connection.app.id
}

data "aws_vpc_endpoint_service" "s3" {
  service      = "s3"
  service_type = "Gateway"
}

output "vpc_endpoint_service" {
  value = data.aws_vpc_endpoint_service.s3.service_name
}

resource "aws_timestreamwrite_database" "metrics" {
  database_name = "tf-metrics"
}

resource "aws_timestreamwrite_table" "cpu" {
  database_name = aws_timestreamwrite_database.metrics.database_name
  table_name    = "cpu"

  retention_properties {
    memory_store_retention_period_in_hours  = 12
    magnetic_store_retention_period_in_days = 30
  }
}

resource "aws_codeartifact_domain" "main" {
  domain = "tf-domain"
}

resource "aws_codeartifact_repository" "npm" {
  repository = "tf-npm"
  domain     = aws_codeartifact_domain.main.domain

  external_connections {
    external_connection_name = "public:npmjs"
  }
}

resource "aws_codeartifact_repository_permissions_policy" "npm" {
  repository = aws_codeartifact_repository.npm.repository
  domain     = aws_codeartifact_domain.main.domain
  policy_document = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = "*"
      Action    = ["codeartifact:ReadFromRepository"]
      Resource  = "*"
    }]
  })
}

# An increase stays PENDING and does not change the informational applied value.
resource "aws_servicequotas_service_quota" "ec2_elastic_ips" {
  service_code = "ec2"
  quota_code   = "L-0263D0A3"
  value        = 10
}

# EMR Serverless is metadata only; this application never provisions workers or runs code.
resource "aws_emrserverless_application" "analytics" {
  name          = "analytics"
  release_label = "emr-7.0.0"
  type          = "Spark"
  tags          = local.tags

  initial_capacity {
    initial_capacity_type = "Driver"
    initial_capacity_config {
      worker_count = 1
      worker_configuration {
        cpu    = "2 vCPU"
        memory = "4 GB"
      }
    }
  }

  maximum_capacity {
    cpu    = "8 vCPU"
    memory = "16 GB"
  }

  auto_start_configuration {
    enabled = false
  }

  auto_stop_configuration {
    enabled              = true
    idle_timeout_minutes = 15
  }
}

# Lake Formation stores records; grants do not change S3 or Glue access.
resource "aws_lakeformation_data_lake_settings" "lake" {
  admins = [aws_iam_role.lambda.arn]
}

resource "aws_lakeformation_resource" "lake" {
  arn                     = aws_s3_bucket.assets.arn
  use_service_linked_role = true
}

resource "aws_lakeformation_permissions" "lake" {
  principal   = aws_iam_role.lambda.arn
  permissions = ["DATA_LOCATION_ACCESS"]
  data_location {
    arn = aws_lakeformation_resource.lake.arn
  }
}

resource "aws_lakeformation_lf_tag" "lake" {
  key    = "tinystack-team"
  values = ["core", "platform"]
}

# Glue orchestration definitions; this trigger stays disabled during provider smoke tests.
resource "aws_glue_workflow" "local" {
  name                   = "tinystack-glue-workflow"
  default_run_properties = { environment = "local" }
  max_concurrent_runs    = 1
  tags                   = local.tags
}

resource "aws_glue_job" "python" {
  name         = "tinystack-glue-python"
  role_arn     = aws_iam_role.lambda.arn
  max_capacity = 0.0625
  timeout      = 1
  command {
    name            = "pythonshell"
    python_version  = "3.9"
    script_location = "s3://${aws_s3_bucket.assets.bucket}/glue/job.py"
  }
  tags = local.tags
}

resource "aws_glue_trigger" "local" {
  name          = "tinystack-glue-start"
  type          = "ON_DEMAND"
  enabled       = false
  workflow_name = aws_glue_workflow.local.name
  actions {
    job_name = aws_glue_job.python.name
  }
  tags = local.tags
}

resource "aws_glue_catalog_database" "lake" {
  name = "tinystack_lake"
}

resource "aws_lakeformation_resource_lf_tags" "lake" {
  database {
    name = aws_glue_catalog_database.lake.name
  }
  lf_tag {
    key   = aws_lakeformation_lf_tag.lake.key
    value = var.team
  }
}

resource "aws_ce_anomaly_monitor" "services" {
  name              = "tinystack-${var.team}"
  monitor_type      = "DIMENSIONAL"
  monitor_dimension = "SERVICE"
  tags              = { team = var.team }
}

resource "aws_ce_anomaly_subscription" "daily" {
  name             = "tinystack-${var.team}"
  frequency        = "DAILY"
  monitor_arn_list = [aws_ce_anomaly_monitor.services.arn]
  threshold_expression {
    dimension {
      key           = "ANOMALY_TOTAL_IMPACT_ABSOLUTE"
      match_options = ["GREATER_THAN_OR_EQUAL"]
      values        = ["10"]
    }
  }
  subscriber {
    type    = "EMAIL"
    address = "${var.team}@example.com"
  }
  tags = { team = var.team }
}

resource "aws_ce_cost_category" "team" {
  name         = "TinyStackTeam"
  rule_version = "CostCategoryExpression.v1"
  rule {
    value = var.team
    rule {
      tags {
        key    = "team"
        values = [var.team]
      }
    }
  }
  default_value = "Other"
  tags          = { team = var.team }
}

# CUR stores preferences only; no billing data or report objects are produced.
resource "aws_cur_report_definition" "local" {
  report_name                = "local-costs"
  time_unit                  = "DAILY"
  format                     = "textORcsv"
  compression                = "GZIP"
  additional_schema_elements = ["RESOURCES"]
  s3_bucket                  = aws_s3_bucket.assets.bucket
  s3_region                  = "us-east-1"
  s3_prefix                  = var.team
  refresh_closed_reports     = true
  report_versioning          = "OVERWRITE_REPORT"
  tags                       = local.tags
}

resource "aws_budgets_budget" "monthly" {
  name         = "monthly"
  budget_type  = "COST"
  limit_amount = "100"
  limit_unit   = "USD"
  time_unit    = "MONTHLY"
  tags         = local.tags

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = var.team == "core" ? 80 : 90
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = var.team == "core" ? ["core@example.com", "remove@example.com"] : ["platform@example.com"]
    subscriber_sns_topic_arns  = [aws_sns_topic.alerts.arn]
  }
}


resource "aws_opensearchserverless_security_policy" "encryption" {
  name = "local-encryption"
  type = "encryption"
  policy = jsonencode({
    Rules       = [{ ResourceType = "collection", Resource = ["collection/local-search"] }]
    AWSOwnedKey = true
  })
}

resource "aws_opensearchserverless_security_policy" "network" {
  name = "local-network"
  type = "network"
  policy = jsonencode([{
    Rules           = [{ ResourceType = "collection", Resource = ["collection/local-search"] }]
    AllowFromPublic = true
  }])
}

resource "aws_opensearchserverless_access_policy" "data" {
  name = "local-data"
  type = "data"
  policy = jsonencode([{
    Rules     = [{ ResourceType = "collection", Resource = ["collection/local-search"], Permission = ["aoss:*"] }]
    Principal = ["arn:aws:iam::000000000000:root"]
  }])
}

resource "aws_opensearchserverless_collection" "search" {
  name             = "local-search"
  description      = "Local search metadata for ${var.team}"
  type             = "SEARCH"
  standby_replicas = "DISABLED"
  tags             = { team = var.team }
  depends_on       = [aws_opensearchserverless_security_policy.encryption]
}

# DMS records only: these resources never migrate data or provision replication compute.
resource "aws_dms_replication_subnet_group" "local" {
  replication_subnet_group_id          = "local-migrations"
  replication_subnet_group_description = "${var.team} migration metadata"
  subnet_ids                           = slice(data.aws_subnets.default.ids, 0, 2)
  tags                                 = local.tags
}

resource "aws_dms_replication_instance" "local" {
  replication_instance_id     = "local-worker"
  replication_instance_class  = "dms.t3.micro"
  replication_subnet_group_id = aws_dms_replication_subnet_group.local.id
  allocated_storage           = var.team == "core" ? 50 : 100
  engine_version              = "3.5.3"
  publicly_accessible         = false
  apply_immediately           = true
  tags                        = local.tags
}

resource "aws_dms_endpoint" "source" {
  endpoint_id   = "local-source"
  endpoint_type = "source"
  engine_name   = "postgres"
  server_name   = "metadata-only.local"
  port          = 5432
  username      = "local"
  password      = "local-records-only"
  database_name = var.team
  tags          = local.tags
}

resource "aws_dms_endpoint" "target" {
  endpoint_id   = "local-target"
  endpoint_type = "target"
  engine_name   = "mysql"
  server_name   = "metadata-only.local"
  port          = 3306
  username      = "local"
  password      = "local-records-only"
  database_name = var.team
  tags          = local.tags
}

resource "aws_dms_replication_task" "local" {
  replication_task_id      = "local-copy"
  replication_instance_arn = aws_dms_replication_instance.local.replication_instance_arn
  source_endpoint_arn      = aws_dms_endpoint.source.endpoint_arn
  target_endpoint_arn      = aws_dms_endpoint.target.endpoint_arn
  migration_type           = "full-load-and-cdc"
  table_mappings = jsonencode({ rules = [{
    rule-type      = "selection", rule-id = "1", rule-name = "all",
    object-locator = { schema-name = "%", table-name = "%" }, rule-action = "include"
  }] })
  start_replication_task = false
  tags                   = local.tags
}

# Records-only Managed Flink application: no job or compute is started.
resource "aws_kinesisanalyticsv2_application" "analytics" {
  name                   = "local-flink"
  runtime_environment    = "FLINK-1_20"
  service_execution_role = aws_iam_role.lambda.arn
  start_application      = true
  tags                   = local.tags

  application_configuration {
    application_code_configuration {
      code_content_type = "ZIPFILE"
      code_content {
        s3_content_location {
          bucket_arn = aws_s3_bucket.assets.arn
          file_key   = aws_s3_object.echo_zip.key
        }
      }
    }
    environment_properties {
      property_group {
        property_group_id = "settings"
        property_map      = { team = var.team }
      }
    }
    flink_application_configuration {
      parallelism_configuration {
        configuration_type   = "CUSTOM"
        parallelism          = 1
        parallelism_per_kpu  = 1
        auto_scaling_enabled = false
      }
    }
    run_configuration {
      application_restore_configuration {
        application_restore_type = "SKIP_RESTORE_FROM_SNAPSHOT"
      }
    }
  }
}

resource "aws_kinesisanalyticsv2_application_snapshot" "analytics" {
  application_name = aws_kinesisanalyticsv2_application.analytics.name
  snapshot_name    = "local-snapshot"
}


resource "aws_signer_signing_profile" "local" {
  name_prefix = "local"
  platform_id = "AWSLambda-SHA384-ECDSA"
  signature_validity_period {
    value = 7
    type  = "DAYS"
  }
  tags = { team = var.team }
}

resource "aws_signer_signing_profile_permission" "local" {
  profile_name = aws_signer_signing_profile.local.name
  action       = "signer:StartSigningJob"
  principal    = "123456789012"
  statement_id = "local"
}


resource "aws_s3_bucket" "signer_output" {
  bucket        = "signer-output"
  force_destroy = true
}

resource "aws_signer_signing_job" "local" {
  profile_name = aws_signer_signing_profile.local.name
  source {
    s3 {
      bucket  = aws_s3_object.hello.bucket
      key     = aws_s3_object.hello.key
      version = aws_s3_object.hello.version_id
    }
  }
  destination {
    s3 {
      bucket = aws_s3_bucket.signer_output.bucket
      prefix = "signed/"
    }
  }
}

resource "aws_waf_ipset" "classic" {
  name = "local-classic-clients"
  ip_set_descriptors {
    type  = "IPV4"
    value = "192.0.2.0/24"
  }
}

resource "aws_waf_byte_match_set" "classic" {
  name = "local-classic-bytes"
  byte_match_tuples {
    text_transformation   = "NONE"
    target_string         = "${var.team}-client"
    positional_constraint = "CONTAINS"
    field_to_match { type = "URI" }
  }
}

resource "aws_waf_rule" "classic" {
  name        = "local-classic-rule"
  metric_name = "LocalClassicRule"
  predicates {
    data_id = aws_waf_ipset.classic.id
    negated = false
    type    = "IPMatch"
  }
  predicates {
    data_id = aws_waf_byte_match_set.classic.id
    negated = false
    type    = "ByteMatch"
  }
  tags = local.tags
}

resource "aws_waf_rate_based_rule" "classic" {
  name        = "local-classic-rate"
  metric_name = "LocalClassicRate"
  rate_key    = "IP"
  rate_limit  = var.team == "core" ? 100 : 200
  predicates {
    data_id = aws_waf_ipset.classic.id
    negated = false
    type    = "IPMatch"
  }
  tags = local.tags
}

resource "aws_waf_web_acl" "classic" {
  name        = "local-classic-acl"
  metric_name = "LocalClassicACL"
  default_action { type = "ALLOW" }
  rules {
    priority = 1
    rule_id  = aws_waf_rule.classic.id
    type     = "REGULAR"
    action { type = "BLOCK" }
  }
  rules {
    priority = 2
    rule_id  = aws_waf_rate_based_rule.classic.id
    type     = "RATE_BASED"
    action { type = "COUNT" }
  }
  tags = local.tags
}

resource "aws_emr_security_configuration" "classic" {
  name = "classic-emr"
  configuration = jsonencode({
    EncryptionConfiguration = {
      EnableInTransitEncryption = false
      EnableAtRestEncryption    = false
    }
  })
}

resource "aws_emr_cluster" "classic" {
  name                              = "classic-emr"
  release_label                     = "emr-7.12.0"
  applications                      = ["Spark"]
  service_role                      = aws_iam_role.eks.arn
  security_configuration            = aws_emr_security_configuration.classic.name
  keep_job_flow_alive_when_no_steps = true
  termination_protection            = false
  tags                              = local.tags

  ec2_attributes {
    subnet_id                         = data.aws_subnets.default.ids[0]
    instance_profile                  = aws_iam_instance_profile.nodes.arn
    emr_managed_master_security_group = aws_security_group.web.id
    emr_managed_slave_security_group  = aws_security_group.web.id
  }

  master_instance_group {
    instance_type  = "m5.xlarge"
    instance_count = 1
  }

  core_instance_group {
    instance_type  = "m5.xlarge"
    instance_count = 2
  }
}

resource "aws_emr_instance_group" "classic" {
  cluster_id     = aws_emr_cluster.classic.id
  instance_type  = "m5.xlarge"
  instance_count = var.team == "core" ? 1 : 2
  name           = "classic-task"
}

resource "aws_amplify_app" "local" {
  name                  = "local-hosting"
  platform              = "WEB"
  build_spec            = "version: 1"
  environment_variables = { TEAM = var.team }
  tags                  = { team = var.team }
}

resource "aws_amplify_branch" "main" {
  app_id                = aws_amplify_app.local.id
  branch_name           = "main"
  stage                 = "DEVELOPMENT"
  environment_variables = { TEAM = var.team }
  tags                  = { team = var.team }
}

resource "aws_accessanalyzer_analyzer" "main" {
  analyzer_name = "main"
  type          = "ACCOUNT"
  tags          = local.tags
}

resource "aws_accessanalyzer_archive_rule" "main" {
  analyzer_name = aws_accessanalyzer_analyzer.main.analyzer_name
  rule_name     = "main"

  filter {
    criteria = "isPublic"
    eq       = ["false"]
  }
}

resource "aws_securityhub_account" "local" {
  enable_default_standards  = false
  control_finding_generator = "SECURITY_CONTROL"
}

resource "aws_securityhub_standards_subscription" "local" {
  depends_on    = [aws_securityhub_account.local]
  standards_arn = "arn:aws:securityhub:us-east-1::standards/aws-foundational-security-best-practices/v/1.0.0"
}

resource "aws_securityhub_action_target" "local" {
  depends_on  = [aws_securityhub_account.local]
  identifier  = "local"
  name        = "${var.team}-review"
  description = "Stored local review action"
}

resource "aws_securityhub_insight" "local" {
  depends_on         = [aws_securityhub_account.local]
  name               = "${var.team}-active"
  group_by_attribute = "SeverityLabel"
  filters {
    record_state {
      comparison = "EQUALS"
      value      = "ACTIVE"
    }
  }
}

resource "aws_networkfirewall_rule_group" "local" {
  capacity = 100
  name     = "local-rg"
  type     = "STATELESS"
  rule_group {
    rules_source {
      stateless_rules_and_custom_actions {
        stateless_rule {
          priority = 1
          rule_definition {
            actions = ["aws:pass"]
            match_attributes {
              destination {
                address_definition = "0.0.0.0/0"
              }
              source {
                address_definition = "0.0.0.0/0"
              }
            }
          }
        }
      }
    }
  }
  tags = local.tags
}

resource "aws_networkfirewall_firewall_policy" "local" {
  name = "local-policy"
  firewall_policy {
    stateless_default_actions          = ["aws:pass"]
    stateless_fragment_default_actions = ["aws:pass"]
    stateless_rule_group_reference {
      priority     = 1
      resource_arn = aws_networkfirewall_rule_group.local.arn
    }
  }
  tags = local.tags
}

resource "aws_networkfirewall_firewall" "local" {
  name                = "local-firewall"
  firewall_policy_arn = aws_networkfirewall_firewall_policy.local.arn
  vpc_id              = aws_vpc.app.id
  subnet_mapping {
    subnet_id = aws_subnet.public.id
  }
  tags = local.tags
}

resource "aws_inspector2_enabler" "local" {
  account_ids    = [data.aws_caller_identity.current.account_id]
  resource_types = ["EC2", "ECR", "LAMBDA", "LAMBDA_CODE"]
}

resource "aws_inspector2_filter" "local" {
  name   = "local-inspector-filter"
  action = "SUPPRESS"
  filter_criteria {
    severity {
      comparison = "EQUALS"
      value      = "LOW"
    }
  }
  tags = { team = var.team }
}

resource "aws_resourceexplorer2_index" "local" {
  type = "LOCAL"
  tags = { team = var.team }
}

resource "aws_resourceexplorer2_view" "local" {
  depends_on = [aws_resourceexplorer2_index.local]
  name       = "local-inventory"
  filters { filter_string = "service:s3" }
  included_property { name = "tags" }
  tags = { team = var.team }
}

resource "aws_servicecatalog_portfolio" "main" {
  name          = "local-portfolio"
  description   = "Records-only catalog"
  provider_name = "TinyStack"
  tags          = { team = var.team }
}

resource "aws_servicecatalog_product" "main" {
  name  = "local-product"
  owner = "TinyStack"
  type  = "CLOUD_FORMATION_TEMPLATE"
  provisioning_artifact_parameters {
    name         = "v1"
    template_url = "https://example.invalid/stored-only.json"
  }
  tags = { team = var.team }
}

resource "aws_servicecatalog_product_portfolio_association" "main" {
  portfolio_id = aws_servicecatalog_portfolio.main.id
  product_id   = aws_servicecatalog_product.main.id
}

resource "aws_servicecatalog_principal_portfolio_association" "main" {
  portfolio_id   = aws_servicecatalog_portfolio.main.id
  principal_arn  = "arn:aws:iam::000000000000:role/catalog-user"
  principal_type = "IAM"
}

resource "aws_globalaccelerator_accelerator" "main" {
  name            = "local-global-accelerator"
  ip_address_type = "IPV4"
  enabled         = false
  tags            = { team = var.team }
}

resource "aws_globalaccelerator_listener" "main" {
  accelerator_arn = aws_globalaccelerator_accelerator.main.id
  protocol        = "TCP"
  client_affinity = "SOURCE_IP"
  port_range {
    from_port = 8080
    to_port   = 8080
  }
}

resource "aws_globalaccelerator_endpoint_group" "main" {
  listener_arn          = aws_globalaccelerator_listener.main.id
  endpoint_group_region = "us-east-1"
  endpoint_configuration {
    endpoint_id = aws_eip.globalaccelerator.id
    weight      = 128
  }
}

resource "aws_eip" "globalaccelerator" {
  domain = "vpc"
}

# FIS executes only the local wait action; no infrastructure faults are injected.
resource "aws_fis_experiment_template" "main" {
  description = "TinyStack local wait"
  role_arn    = aws_iam_role.lambda.arn
  stop_condition {
    source = "none"
  }
  action {
    name      = "wait"
    action_id = "aws:fis:wait"
    parameter {
      key   = "duration"
      value = "PT1M"
    }
  }
  tags = local.tags
}

resource "aws_iam_role" "datasync" {
  name = "datasync"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "datasync.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_datasync_location_s3" "source" {
  s3_bucket_arn = aws_s3_bucket.assets.arn
  subdirectory  = "/input/"
  s3_config {
    bucket_access_role_arn = aws_iam_role.datasync.arn
  }
  tags = local.tags
}

resource "aws_datasync_location_s3" "destination" {
  s3_bucket_arn = aws_s3_bucket.signer_output.arn
  subdirectory  = "/output/"
  s3_config {
    bucket_access_role_arn = aws_iam_role.datasync.arn
  }
  tags = local.tags
}

resource "aws_datasync_task" "local" {
  name                     = "local-transfer"
  source_location_arn      = aws_datasync_location_s3.source.arn
  destination_location_arn = aws_datasync_location_s3.destination.arn
  options {
    verify_mode   = "ONLY_FILES_TRANSFERRED"
    transfer_mode = "CHANGED"
  }
  tags = local.tags
}

# Subscription destruction leaves the annual commitment intact.
resource "aws_shield_subscription" "local" {
  skip_destroy = true
}

resource "aws_shield_protection" "local" {
  name         = "local-eip"
  resource_arn = "arn:aws:ec2:us-east-1:${data.aws_caller_identity.current.account_id}:eip-allocation/${aws_eip.nat.id}"
  tags         = local.tags
  depends_on   = [aws_shield_subscription.local]
}

resource "aws_shield_protection_group" "local" {
  protection_group_id = "local-protections"
  aggregation         = "SUM"
  pattern             = "ARBITRARY"
  members             = [aws_shield_protection.local.resource_arn]
  tags                = local.tags
  depends_on          = [aws_shield_protection.local]
}

resource "aws_swf_domain" "main" {
  name                                        = "local-workflows"
  workflow_execution_retention_period_in_days = 1
  tags                                        = { team = var.team }
}

# Provider 5.100 updates tags in place; deletion protection must be disabled for destroy.
resource "aws_dsql_cluster" "local" {
  deletion_protection_enabled = false
  tags                        = local.tags
}

# Both provider aliases use the same local endpoint; SigV4 carries the Region.
provider "aws" {
  alias                       = "dsql_peer"
  access_key                  = "test"
  secret_key                  = "test"
  region                      = "us-east-2"
  skip_metadata_api_check     = true
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  endpoints {
    dsql = var.endpoint
    sts  = var.endpoint
  }
}

resource "aws_dsql_cluster" "primary" {
  deletion_protection_enabled = false
  multi_region_properties {
    witness_region = "us-west-2"
  }
  tags = local.tags
}

resource "aws_dsql_cluster" "peer" {
  provider                    = aws.dsql_peer
  deletion_protection_enabled = false
  multi_region_properties {
    witness_region = "us-west-2"
  }
  tags = local.tags
}

# Reciprocal peer updates run concurrently after both regional records exist.
resource "aws_dsql_cluster_peering" "primary" {
  identifier     = aws_dsql_cluster.primary.identifier
  witness_region = "us-west-2"
  clusters       = [aws_dsql_cluster.peer.arn]
}

resource "aws_dsql_cluster_peering" "peer" {
  provider       = aws.dsql_peer
  identifier     = aws_dsql_cluster.peer.identifier
  witness_region = "us-west-2"
  clusters       = [aws_dsql_cluster.primary.arn]
}

resource "aws_media_convert_queue" "local" {
  name         = "tinystack-media"
  description  = "Local lifecycle queue"
  pricing_plan = "ON_DEMAND"
  status       = "ACTIVE"
  tags         = local.tags
}

resource "aws_glue_classifier" "csv" {
  name = "tinystack-glue-csv"
  csv_classifier {
    contains_header = "PRESENT"
    delimiter       = ","
    quote_symbol    = "\""
  }
}

resource "aws_glue_crawler" "s3" {
  name          = "tinystack-glue-crawler"
  role          = aws_iam_role.lambda.arn
  database_name = aws_glue_catalog_database.lake.name
  classifiers   = [aws_glue_classifier.csv.name]
  s3_target {
    path = "s3://${aws_s3_bucket.assets.bucket}/glue/data/"
  }
  tags = local.tags
}

resource "aws_transcribe_vocabulary" "fixtures" {
  vocabulary_name = "fixture-vocabulary"
  language_code   = "en-US"
  phrases         = ["TinyStack"]
  tags            = { team = var.team }
}

resource "aws_transcribe_vocabulary_filter" "fixtures" {
  vocabulary_filter_name = "fixture-filter"
  language_code          = "en-US"
  words                  = ["example"]
  tags                   = { team = var.team }
}

# Bedrock Agents: tags on agent, alias, and knowledge base update in place.
resource "aws_bedrockagent_agent" "assistant" {
  agent_name                  = "local-assistant"
  agent_resource_role_arn     = aws_iam_role.eks.arn
  foundation_model            = "anthropic.claude-sonnet-4-5-20250929-v1:0"
  instruction                 = "Use only the user supplied fixtures for deterministic local responses."
  idle_session_ttl_in_seconds = 600
  skip_resource_in_use_check  = true
  tags                        = local.tags
}

resource "aws_bedrockagent_agent_action_group" "input" {
  agent_id                      = aws_bedrockagent_agent.assistant.agent_id
  agent_version                 = "DRAFT"
  action_group_name             = "user-input"
  parent_action_group_signature = "AMAZON.UserInput"
  skip_resource_in_use_check    = true
}

resource "aws_bedrockagent_agent_alias" "assistant" {
  agent_id         = aws_bedrockagent_agent.assistant.agent_id
  agent_alias_name = "production"
  depends_on       = [aws_bedrockagent_agent_action_group.input]
  tags             = local.tags
}

resource "aws_bedrockagent_knowledge_base" "documents" {
  name     = "local-documents"
  role_arn = aws_iam_role.eks.arn
  knowledge_base_configuration {
    type = "VECTOR"
    vector_knowledge_base_configuration {
      embedding_model_arn = "arn:aws:bedrock:us-east-1::foundation-model/amazon.titan-embed-text-v2:0"
    }
  }
  storage_configuration {
    type = "OPENSEARCH_SERVERLESS"
    opensearch_serverless_configuration {
      collection_arn    = aws_opensearchserverless_collection.search.arn
      vector_index_name = "documents"
      field_mapping {
        vector_field   = "vector"
        text_field     = "text"
        metadata_field = "metadata"
      }
    }
  }
  tags = local.tags
}

# Bedrock AgentCore: records mode stores the runtime without running its image. A description change creates a new
# runtime version, which the DEFAULT endpoint follows; the named endpoint stays on version 1.
resource "aws_bedrockagentcore_agent_runtime" "assistant" {
  agent_runtime_name = "local_assistant"
  description        = "Owned by ${var.team}"
  role_arn           = aws_iam_role.eks.arn
  agent_runtime_artifact {
    container_configuration {
      container_uri = "000000000000.dkr.ecr.us-east-1.amazonaws.com/assistant:latest"
    }
  }
  network_configuration {
    network_mode = "PUBLIC"
  }
  environment_variables = { TEAM = var.team }
  tags                  = local.tags
}

resource "aws_bedrockagentcore_agent_runtime_endpoint" "assistant" {
  name                  = "production"
  agent_runtime_id      = aws_bedrockagentcore_agent_runtime.assistant.agent_runtime_id
  agent_runtime_version = "1"
  tags                  = local.tags
}

resource "aws_bedrockagentcore_memory" "assistant" {
  name                  = "local_memory"
  event_expiry_duration = 30
  tags                  = local.tags
}
