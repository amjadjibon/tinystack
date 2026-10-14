// A small CDK app: a queue, a bucket, and a function whose code is a file asset.
const cdk = require("aws-cdk-lib");
const lambda = require("aws-cdk-lib/aws-lambda");
const s3 = require("aws-cdk-lib/aws-s3");
const sqs = require("aws-cdk-lib/aws-sqs");

const app = new cdk.App();
const account = process.env.CDK_DEFAULT_ACCOUNT;
const stack = new cdk.Stack(app, "TinyStackCdk", {
  env: { account, region: process.env.CDK_DEFAULT_REGION },
});

const queue = new sqs.Queue(stack, "Jobs", { queueName: "cdk-jobs" });
const bucket = new s3.Bucket(stack, "Files", { bucketName: `cdk-files-${account}` });
const handler = new lambda.Function(stack, "Handler", {
  runtime: lambda.Runtime.NODEJS_22_X,
  handler: "index.handler",
  code: lambda.Code.fromAsset("handler"),
  environment: { QUEUE_URL: queue.queueUrl, BUCKET: bucket.bucketName },
});
queue.grantSendMessages(handler);

new cdk.CfnOutput(stack, "QueueUrl", { value: queue.queueUrl });
new cdk.CfnOutput(stack, "BucketName", { value: bucket.bucketName });
new cdk.CfnOutput(stack, "FunctionName", { value: handler.functionName });
