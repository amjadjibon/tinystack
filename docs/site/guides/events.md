# Test an event-driven workflow

## Fan out SNS to SQS

With TinyStack running and wrappers built (`task build:tools`):

```sh
QUEUE_URL=$(./bin/aws-local sqs create-queue --queue-name docs-events \
  --query QueueUrl --output text)
QUEUE_ARN=$(./bin/aws-local sqs get-queue-attributes --queue-url "$QUEUE_URL" \
  --attribute-names QueueArn --query Attributes.QueueArn --output text)
TOPIC_ARN=$(./bin/aws-local sns create-topic --name docs-events \
  --query TopicArn --output text)

./bin/aws-local sns subscribe --topic-arn "$TOPIC_ARN" \
  --protocol sqs --notification-endpoint "$QUEUE_ARN"
./bin/aws-local sns publish --topic-arn "$TOPIC_ARN" --message hello
./bin/aws-local sqs receive-message --queue-url "$QUEUE_URL" --wait-time-seconds 1
```

The SQS message contains an SNS envelope whose `Message` is `hello`. Use the subscription's
`RawMessageDelivery` setting when your workflow expects the original body without an envelope.

## Inspect the hand-off

```sh
curl -fsS http://127.0.0.1:4566/_tinystack/deliveries
```

The delivery log shows source, target, payload and outcome. A successful publish can still have a failed
local downstream delivery.

## Extend the workflow

| Workflow | Configure | Verify |
| --- | --- | --- |
| S3 → SQS/SNS/Lambda/EventBridge | Bucket notifications and supported destination | Object write produces the event |
| EventBridge → SQS/SNS/Lambda | Rule pattern, target and optional input transformer | A matching event reaches the target |
| SQS/Streams/Kinesis → Lambda | Function and event source mapping | Invocation and acknowledgement/cursor behavior |
| Kinesis → Firehose → S3 | Source and S3 delivery stream | Buffered records appear in an object |
| IoT → other services | Topic SQL and implemented action | MQTT publish reaches the action |
| CloudWatch → SNS/Lambda/Auto Scaling | Alarm and supported action | State change triggers its effect |

[Event architecture](../architecture/events-runtimes.md) explains the in-process paths. See each
[service page](../services/index.md) for target and retry limits.
Some first deliveries are synchronous; others use workers, pollers or buffering. Assert target results and
inspect failures rather than assuming every accepted message completes immediately.
