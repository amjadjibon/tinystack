# Interactive console tutorials

Open **Tutorials** in the top navigation, the **Get started** widget on console home (add it back with **Add widgets** if you removed it), or `/tutorials`.
Choose an account and Region, then **Start**. Tutorials require their services to be registered; unavailable
services disable Start. The drawer shows permissions, generated resource names, instructions, and copyable CLI
commands. **Go there** opens the relevant service page and preserves account and Region. Numbered hotspots
point to real console controls. **Next** and **Previous** in the drawer allow manual navigation. Drag the
drawer edge to resize it; the console remembers the width.

The current step checks live state every two seconds, including while the drawer is closed. Creating resources
or running commands in CloudShell or an AWS CLI advances the same tutorial. Only sources used by that step are
polled. Failed API calls do not count. The S3 upload check uses a successful `PutObject` request because the
inventory exposes buckets, not individual object keys. The download check uses a successful `GetObject`.
Request/delivery logs are bounded: if a record has expired or was cleared, repeat the action or choose Next.

Live diagnostics require administrator access when console sign-in is configured. A 403 shows
**Administrator access required** and leaves manual Next available. Service actions still use your signed-in
principal and its permissions. Tutorials never bypass authorization.

## Store and retrieve a file in S3

1. Start the S3 tutorial. Choose Go there, then **Create bucket**. Copy the generated bucket name; leave
   versioning disabled.
2. Open that bucket, choose **Upload**, and upload a local file named `hello.txt` (up to the console's 1 MiB limit).
3. Open the object's detail page and choose **Download**. The successful GetObject completes the tutorial.

For a CLI run, restart the tutorial to obtain a new generated name. Open CloudShell and use the step's copied
commands individually in order. CloudShell accepts AWS commands only: no variables, exports, redirects, or shell substitutions. The copied S3 upload command creates an empty hello.txt; use a file on the host with `--body /absolute/path/hello.txt` to upload content. Tutorial commands use `aws-local` (built by `task build:tools`; see
[clients](../getting-started/clients.md)), which always targets the local gateway, so a copied command cannot reach
AWS or incur charges. In an external terminal, set `AWS_ACCESS_KEY_ID` to credentials belonging to the selected
account (otherwise `aws-local` uses `test`) and `AWS_DEFAULT_REGION` to the selected Region. The following script is for an external terminal, with `BUCKET` replaced by the generated name:

```sh
# us-east-1; for another Region, add --create-bucket-configuration LocationConstraint=$AWS_DEFAULT_REGION
aws-local s3api create-bucket --bucket "$BUCKET"
printf 'Hello TinyStack' > /tmp/hello.txt
aws-local s3api put-object --bucket "$BUCKET" --key hello.txt --body /tmp/hello.txt
aws-local s3api get-object --bucket "$BUCKET" --key hello.txt /tmp/tutorial-download.txt
```

Wait for each step to advance before running the next command. Finish with **Clean up**, inspect the listed
object and bucket, type `delete`, and confirm. Only `hello.txt` is deleted; a bucket with additional objects,
versions, or delete markers fails explicitly rather than emptying unrelated data.

## Fan out messages with SNS and SQS

1. Start the SNS/SQS tutorial. Create a **Standard** SNS topic using its generated name.
2. Create a **Standard** SQS queue with the suggested queue name.
3. Return to the topic's **Subscriptions** tab, choose **Create subscription**, select SQS, and use the generated
   queue ARN shown in the instructions. If authorization is enforced, use the queue's **Permissions** tab to
   permit `sns.amazonaws.com` to perform `sqs:SendMessage`, restricted by `aws:SourceArn` to your topic ARN.
4. Open the topic's **Publish message** tab and publish `Hello TinyStack`. The step waits for successful delivery
   to this queue, rather than only acceptance of the publish request.
5. Open the queue's **Send and receive messages** tab and choose **Poll for messages**. Receiving a nonempty
   result completes the tutorial.

For an external CLI run, restart, substitute the generated name as `NAME`, and set `ACCOUNT` and `AWS_DEFAULT_REGION`
to the selected scope. In CloudShell the local endpoint and credentials are supplied. An external shell needs
`aws-local` and credentials for that account. Run each operation after the drawer reaches its step:

```sh
TOPIC_ARN="arn:aws:sns:$AWS_DEFAULT_REGION:$ACCOUNT:$NAME"
QUEUE_ARN="arn:aws:sqs:$AWS_DEFAULT_REGION:$ACCOUNT:$NAME"
aws-local sns create-topic --name "$NAME"
aws-local sqs create-queue --queue-name "$NAME"
QUEUE_URL=$(aws-local sqs get-queue-url --queue-name "$NAME" --query QueueUrl --output text)
# This queue policy also allows the delivery in enforced authorization mode.
POLICY=$(printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"sns.amazonaws.com"},"Action":"sqs:SendMessage","Resource":"%s","Condition":{"ArnEquals":{"aws:SourceArn":"%s"}}}]}' "$QUEUE_ARN" "$TOPIC_ARN")
# Produce a JSON attributes file with the Policy attribute encoded as a JSON string.
POLICY="$POLICY" python3 -c 'import json,os; print(json.dumps({"Policy":os.environ["POLICY"]}))' > /tmp/tutorial-queue-policy.json
aws-local sqs set-queue-attributes --queue-url "$QUEUE_URL" --attributes file:///tmp/tutorial-queue-policy.json
aws-local sns subscribe --topic-arn "$TOPIC_ARN" --protocol sqs --notification-endpoint "$QUEUE_ARN"
aws-local sns publish --topic-arn "$TOPIC_ARN" --message 'Hello TinyStack'
aws-local sqs receive-message --queue-url "$QUEUE_URL"
```

For CloudShell, run the single expanded AWS commands from the drawer, one at a time. Use the queue Permissions
editor for the policy. Its receive command uses TinyStack's supported path-only queue URL, so no shell variable
is needed. The script above is for an external terminal: CloudShell does not launch a host shell and cannot run
Python, assignments, redirection, or command substitutions. Cleanup confirms exact subscription ARNs, the generated topic, and the queue (including
its messages). Operations execute in order; a failure identifies the resource and server code/message. Retry
continues unfinished operations.

## Store and read items in DynamoDB

1. Start the DynamoDB tutorial. Choose **Create table**, use the generated table name, and keep the partition key
   `pk` of type String with on-demand capacity.
2. Open the table, choose **Create item**, and save `{"pk": {"S": "hello"}, "message": {"S": "Hello TinyStack"}}`.
   A successful `PutItem` to this table completes the step.
3. Keep **Scan** selected and choose **Run**, or run a Query on `pk = :pk`. The step completes when a Scan, Query or
   GetItem on this table returns at least one item; an empty result does not count.

From a terminal, replace `TABLE` with the generated name:

```sh
aws-local dynamodb create-table --table-name "$TABLE" --attribute-definitions AttributeName=pk,AttributeType=S \
  --key-schema AttributeName=pk,KeyType=HASH --billing-mode PAY_PER_REQUEST
aws-local dynamodb put-item --table-name "$TABLE" --item '{"pk":{"S":"hello"},"message":{"S":"Hello TinyStack"}}'
aws-local dynamodb get-item --table-name "$TABLE" --key '{"pk":{"S":"hello"}}'
```

Cleanup deletes the generated table and every item in it.

## Route events to SQS with EventBridge

1. Start the EventBridge tutorial. Create a **Standard** SQS queue with the generated name.
2. Open EventBridge **Rules** on the `default` bus, choose **Create rule**, use the generated rule name, and keep the
   event pattern `{"source":["my.app"]}`. A rule with that name on another bus does not count.
3. Open the rule, choose **Add target**, and enter the queue ARN shown in the instructions. If authorization is
   enforced, use the queue's **Permissions** tab to permit `events.amazonaws.com` to perform `sqs:SendMessage`,
   restricted by `aws:SourceArn` to the rule ARN.
4. Choose **Send event** and keep source `my.app`. The step waits for the rule's successful delivery to this queue,
   not only for `PutEvents` to be accepted.
5. Open the queue's **Send and receive messages** tab and choose **Poll for messages**. The body is the full event.

From a terminal, replace `NAME` with the generated name and set `ACCOUNT` and `AWS_DEFAULT_REGION` to the selected
scope (in CloudShell, run the drawer's expanded commands instead):

```sh
aws-local sqs create-queue --queue-name "$NAME"
aws-local events put-rule --name "$NAME" --event-pattern '{"source":["my.app"]}'
aws-local events put-targets --rule "$NAME" --targets "Id=queue,Arn=arn:aws:sqs:$AWS_DEFAULT_REGION:$ACCOUNT:$NAME"
aws-local events put-events --entries '[{"Source":"my.app","DetailType":"Tutorial","Detail":"{}"}]'
aws-local sqs receive-message --queue-url "/$ACCOUNT/$NAME"
```

Cleanup removes every target of the generated rule, then deletes the rule and the queue with its messages.

## Saved progress

Progress, completed steps, generated names, and completion time are stored in this browser per tutorial,
account, and Region. Return to `/tutorials` and choose **Resume** after a reload. Switch back to the original
scope to resume its progress. **Restart** replaces that tutorial's progress with fresh names; it does not delete
previous resources. Clean up before restarting when possible. A successful reset from **Developer tools** clears
all tutorial progress in this browser. Browser storage restrictions limit persistence to the current session.

After completion the drawer shows a summary, Learn more, and cleanup. Other names can be used with manual
Next, but cleanup targets only the names generated for that tutorial. Resetting through a separate CLI or another
browser does not clear this browser's saved tutorial progress.
