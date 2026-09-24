# DynamoDB wire parity

Run the checked-in contract baseline offline:

```sh
go test ./tests/parity -run '^TestDynamoDBParity$'
```

`dynamodb/scenario.json` sends the same AWS JSON 1.0 requests to each endpoint.
`dynamodb/expected.json` checks status, content type, presence of a request ID,
selected response bodies, the conditional error code, and the item state after
that error. Its provenance field says `contract baseline`: these expectations
have not yet been captured from a live AWS account.

With valid AWS credentials, capture a live snapshot:

```sh
AWS_REGION=us-east-1 \
TINYSTACK_PARITY_CAPTURE=1 \
TINYSTACK_PARITY_OUTPUT=/tmp/dynamodb-aws-response.json \
go test ./tests/parity -run '^TestCaptureDynamoDBAgainstAWS$' -count=1 -v
```

The capture test requires `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (and
uses `AWS_SESSION_TOKEN` when set). It creates a uniquely named DynamoDB table,
waits for ACTIVE, runs the scenario, writes the full responses to the chosen
file, and deletes the table. Review the captured bodies before promoting their
stable fields into `expected.json`, changing the provenance to the AWS region
and capture date. Keep generated IDs, timestamps, and error wording out of the
offline comparison unless they are the behavior under test.
