# Interactive console tutorials

Open **Tutorials** in the top navigation, the **Get started** card on console home, or `/tutorials`.
Choose an account and Region, then **Start**. Tutorials require their services to be registered; unavailable
services disable Start. The drawer shows permissions, generated resource names, instructions, and copyable CLI
commands. **Go there** opens the relevant service page and preserves account and Region. Numbered hotspots
point to real console controls. **Next** and **Previous** in the drawer allow manual navigation.

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
commands individually in order. CloudShell accepts AWS commands only: no variables, exports, redirects, or shell substitutions. The copied S3 upload command creates an empty hello.txt; use a file on the host with `--body /absolute/path/hello.txt` to upload content. In an external terminal, first set `AWS_ENDPOINT_URL` to the local gateway URL and use
credentials belonging to the selected account. Set `AWS_DEFAULT_REGION` to the selected Region. Never point
these commands at AWS. The following script is for an external terminal, with `BUCKET` replaced by the generated name:

```sh
# us-east-1; for another Region, add --create-bucket-configuration LocationConstraint=$AWS_DEFAULT_REGION
aws s3api create-bucket --bucket "$BUCKET"
printf 'Hello TinyStack' > /tmp/hello.txt
aws s3api put-object --bucket "$BUCKET" --key hello.txt --body /tmp/hello.txt
aws s3api get-object --bucket "$BUCKET" --key hello.txt /tmp/tutorial-download.txt
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
`AWS_ENDPOINT_URL` and credentials for that account. Run each operation after the drawer reaches its step:

```sh
TOPIC_ARN="arn:aws:sns:$AWS_DEFAULT_REGION:$ACCOUNT:$NAME"
QUEUE_ARN="arn:aws:sqs:$AWS_DEFAULT_REGION:$ACCOUNT:$NAME"
aws sns create-topic --name "$NAME"
aws sqs create-queue --queue-name "$NAME"
QUEUE_URL=$(aws sqs get-queue-url --queue-name "$NAME" --query QueueUrl --output text)
# This queue policy also allows the delivery in enforced authorization mode.
POLICY=$(printf '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"sns.amazonaws.com"},"Action":"sqs:SendMessage","Resource":"%s","Condition":{"ArnEquals":{"aws:SourceArn":"%s"}}}]}' "$QUEUE_ARN" "$TOPIC_ARN")
# Produce a JSON attributes file with the Policy attribute encoded as a JSON string.
POLICY="$POLICY" python3 -c 'import json,os; print(json.dumps({"Policy":os.environ["POLICY"]}))' > /tmp/tutorial-queue-policy.json
aws sqs set-queue-attributes --queue-url "$QUEUE_URL" --attributes file:///tmp/tutorial-queue-policy.json
aws sns subscribe --topic-arn "$TOPIC_ARN" --protocol sqs --notification-endpoint "$QUEUE_ARN"
aws sns publish --topic-arn "$TOPIC_ARN" --message 'Hello TinyStack'
aws sqs receive-message --queue-url "$QUEUE_URL"
```

For CloudShell, run the single expanded AWS commands from the drawer, one at a time. Use the queue Permissions
editor for the policy. Its receive command uses TinyStack's supported path-only queue URL, so no shell variable
is needed. The script above is for an external terminal: CloudShell does not launch a host shell and cannot run
Python, assignments, redirection, or command substitutions. Cleanup confirms exact subscription ARNs, the generated topic, and the queue (including
its messages). Operations execute in order; a failure identifies the resource and server code/message. Retry
continues unfinished operations.

## Saved progress

Progress, completed steps, generated names, and completion time are stored in this browser per tutorial,
account, and Region. Return to `/tutorials` and choose **Resume** after a reload. Switch back to the original
scope to resume its progress. **Restart** replaces that tutorial's progress with fresh names; it does not delete
previous resources. Clean up before restarting when possible. A successful reset from **Developer tools** clears
all tutorial progress in this browser. Browser storage restrictions limit persistence to the current session.

After completion the drawer shows a summary, Learn more, and cleanup. Other names can be used with manual
Next, but cleanup targets only the names generated for that tutorial. Resetting through a separate CLI or another
browser does not clear this browser's saved tutorial progress.
