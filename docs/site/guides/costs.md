# Cost fixtures and queries

TinyStack calculates actual spend only from user-supplied fixtures. It does not estimate prices from resources,
requests or elapsed time, and never calls AWS billing APIs. These values are local test data, not an AWS invoice.

## Fixture API

All paths start with `/_tinystack/costs/fixtures`. They target **`defaults.account`**, regardless of signing Region
or credential account. Query parameters, including `account`, are rejected. This follows the existing management
endpoint boundary: in console SSO mode every method requires a SigV4 request for service `tinystack`, signed by
configured root credentials or an administrator's SSO session. Ordinary IAM and nonadministrator SSO credentials
cannot use it. Outside SSO mode it is open like the fault endpoint. Writes reject a different-host `Origin`.

- `PUT /<id>`: atomically replace the complete named dataset, or create it; returns 200 and the stored fixture.
  Each successful PUT generates a new opaque revision, even if the body is identical. Validation errors leave
  the old dataset untouched. Different fixture IDs add together; line IDs are unique only within a fixture.
- `GET`: returns 200 with `{"fixtures":[{"id":"demo","revision":"...","lineCount":1,
  "provenance":"manual test","updatedAt":"2026-10-01T00:00:00Z"}]}` in fixture-ID order. Empty list is `[]`.
- `GET /<id>`: returns 200 with the stored fixture, or 404. Its object has the input fields below plus `id`,
  `revision`, `lineCount` and `updatedAt` (UTC RFC3339 timestamp). Defaulted `linkedAccount` is included in each line.
- `DELETE /<id>`: returns 204 without a body, or 404 when absent.

Invalid input returns 400 with `{"message":"..."}`; forbidden access returns 403; unsupported methods return 405.
Storage read failures return 500. Responses use JSON and `Cache-Control: no-store` (204/405 have no JSON body).
`POST /_tinystack/reset` clears all fixtures. Persistent storage retains data, revisions and provenance on restart.

### Exact input contract

The [JSON Schema](../assets/cost-fixture.schema.json) describes the PUT payload; timestamp, cross-field and UTF-8
byte constraints are enforced by the server as detailed below.

PUT accepts one JSON object with **only** `provenance` and `lines`. Unknown fields and null values at every level fail explicitly.
`lines` is required and must be an array; `[]` is valid. `provenance` is an optional string, default `""`, at most
4096 bytes. Fixture IDs and line IDs match `^[A-Za-z0-9_-]{1,128}$`.

Each line is an object with these fields (all other fields are rejected):

| Field | JSON type | Requirement |
| --- | --- | --- |
| `id` | string | Required; unique within this fixture |
| `start` | string | Required; inclusive UTC `YYYY-MM-DD` or RFC3339 timestamp with zero UTC offset |
| `end` | string | Required; exclusive, strictly after start, at or before the next UTC midnight |
| `service` | string | Required, 1–1024 bytes; AWS Cost Explorer SERVICE label, e.g. `Amazon Simple Storage Service` |
| `region` | string | Optional, default `""`, at most 256 bytes |
| `linkedAccount` | string | Optional; omitted/empty defaults to `defaults.account`; any other account fails |
| `usageType` | string | Optional, default `""`, at most 1024 bytes |
| `tags` | object of string values | Optional; at most 50 entries; keys 1–128 bytes, values at most 256 bytes |
| `unblendedCost` | string | Required; `^-?(0|[1-9][0-9]{0,29})(\.[0-9]{1,12})?$`; negative credits allowed |
| `currency` | string | Required; exactly `USD` |
| `estimated` | boolean | Optional, default `false` |

Limits: **100 fixtures per account**, **10,000 lines per fixture**, **2 MiB request body**. Dates and times are UTC.
A line belongs wholly to the period containing `start`; it is never prorated. A date-to-next-date line is valid;
its exclusive end is next midnight. Money uses exact decimal arithmetic with up to 12 fractional digits; responses
remove trailing zeroes (`10.10` becomes `10.1`). Credits retain their sign.

### Full example and loading

Save this as `costs.json`:

```json
{
  "provenance": "hand-authored checkout test",
  "lines": [
    {
      "id": "s3-charge",
      "start": "2026-10-01",
      "end": "2026-10-02",
      "service": "Amazon Simple Storage Service",
      "region": "us-east-1",
      "linkedAccount": "000000000000",
      "usageType": "TimedStorage-ByteHrs",
      "tags": {"team": "checkout"},
      "unblendedCost": "12.345",
      "currency": "USD",
      "estimated": true
    },
    {
      "id": "credit",
      "start": "2026-10-01T12:00:00Z",
      "end": "2026-10-01T13:00:00Z",
      "service": "Amazon Simple Storage Service",
      "unblendedCost": "-2.345",
      "currency": "USD"
    }
  ]
}
```

```sh
curl --fail -X PUT "$TINYSTACK_ENDPOINT_URL/_tinystack/costs/fixtures/checkout" \
  -H 'Content-Type: application/json' --data-binary @costs.json
curl --fail "$TINYSTACK_ENDPOINT_URL/_tinystack/costs/fixtures"
curl --fail -X DELETE "$TINYSTACK_ENDPOINT_URL/_tinystack/costs/fixtures/checkout"
```

For an [init hook](init-hooks.md), put the PUT command in an executable `ready.d/10-costs.sh`, using an absolute
path for `costs.json` and setting `TINYSTACK_ENDPOINT_URL` to your listener. With SSO enabled, sign it for
`tinystack` using the administrator credentials (for example curl's `--aws-sigv4 aws:amz:us-east-1:tinystack`,
`--user "$AWS_ACCESS_KEY_ID:$AWS_SECRET_ACCESS_KEY"`, plus `X-Amz-Security-Token` for a session).

## Cost Explorer

Three additional AWS JSON 1.1 operations read the account-wide fixtures, independent of signing Region:

- `GetCostAndUsage`: `DAILY`/`MONTHLY`, exactly `Metrics=["UnblendedCost"]`, date-only inclusive Start and exclusive
  End, at most ten years. Partial first/last monthly periods are clipped to the query boundaries. Each requested
  period appears: no data gives zero ungrouped totals or no groups. This is a deliberate local empty-data rule;
  the [AWS API reference](https://docs.aws.amazon.com/aws-cost-management/latest/APIReference/API_GetCostAndUsage.html)
  does not specify empty-data results. Estimated is true if any matching line in that period is estimated.
- Filters: `SERVICE`, `LINKED_ACCOUNT`, `REGION`, `USAGE_TYPE`, and tags; exact, case-sensitive membership matching
  (values OR together); `And`, `Or`, `Not` up to 32 nested levels. Exactly one operator per expression. MatchOptions
  must be absent, empty or `["EQUALS"]`. Missing tag keys do not match. Cost categories are rejected.
- GroupBy: at most two distinct keys of type `DIMENSION` (the four above) or `TAG`. Tag groups use `key$value`,
  including `key$` for absent tags. Group keys are deterministic. Grouped results omit Total. No account name or
  dimension attributes are invented: `DimensionValueAttributes` is absent and dimension-value Attributes are absent.
- Cost query pagination returns at most 31 **periods** per page, including all groups in those periods. Follow
  `NextPageToken` using the unchanged query. Tokens are deterministic and bound to the account, query and all
  fixture revisions; changes, replacements, additions and deletions invalidate them with `InvalidNextTokenException`.
- `GetDimensionValues`: the four dimensions above, TimePeriod, optional Filter, SearchString (case-sensitive
  substring), Context absent or `COST_AND_USAGE`, MaxResults 1–1000 (default 100), and NextPageToken. Missing/empty
  dimension values are excluded. Values are sorted, ReturnSize is page size, TotalSize is the full match count.
- `GetTags`: TimePeriod, optional Filter, SearchString, MaxResults and NextPageToken as above. Without TagKey,
  returns keys; with a nonempty TagKey, returns its distinct values, including empty values. Values are sorted.

Other metrics, hourly/resource queries, forecasts, billing views, sorting, cost-category evaluation, anomaly
queries, RI/SP calculations and recommendations remain unsupported and fail explicitly. Stored monitors,
subscriptions and categories remain records; ingesting fixtures does not evaluate them or deliver notifications.

## Budget actual spend

`DescribeBudget` and `DescribeBudgets` include `CalculatedSpend.ActualSpend` for COST budgets with a USD
BudgetLimit or all-USD PlannedBudgetLimits, TimeUnit MONTHLY, QUARTERLY or ANNUALLY, no explicit CostTypes, and absent CostFilters or filters using
the same four dimensions (AND across keys, OR across values). Unsupported filter configurations, explicit
CostTypes, other budget types/units and inactive budgets omit CalculatedSpend.

The current UTC calendar month, quarter or year is intersected with the budget's optional TimePeriod and clipped
to now. Only lines whose start is within that period contribute. No fixture data means actual spend `"0"` for a
supported budget. There is no ForecastedSpend or automatic price calculation. Supported ACTUAL notifications are
evaluated as described below.
CalculatedSpend is read-only and rejected on budget writes.

## Budget alerts

TinyStack checks every indexed budget once per second, reading its definitions and account's current fixtures in
one transaction. Fixture PUT/DELETE and budget, notification and subscriber edits are picked up on the next tick.
There is no separate evaluation endpoint. Only the COST/USD/unblended MONTHLY, QUARTERLY and ANNUALLY budgets
supported by ActualSpend above are evaluated. Other definitions and their notifications remain stored without
execution. FORECASTED notifications never fire: TinyStack has no forecasts.

PERCENTAGE compares actual spend against `limit × threshold × 0.01`; ABSOLUTE_VALUE uses the threshold amount.
The comparisons are strictly `>` for GREATER_THAN, strictly `<` for LESS_THAN, and exactly `=` for EQUAL_TO,
including the equality boundary for EQUAL_TO. Calculations use decimal rationals, never floating-point money or
division by the budget limit. The API's modeled double threshold enters this calculation as its shortest decimal
representation. A zero limit is valid. Planned limits select the latest timestamp not after evaluation time,
carrying the last amount forward. Percentage alerts wait for an applicable limit. Absolute alerts can fire without
one; their message reports the budgeted amount as unavailable.
These comparisons follow the [AWS Notification contract](https://docs.aws.amazon.com/aws-cost-management/latest/APIReference/API_budgets_Notification.html),
whose example describes going over $160 on a $200 budget at 80 percent.

Each notification identity (NotificationType, ComparisonOperator, Threshold, ThresholdType) fires once per budget
period. NotificationState is ALARM after firing in the current period, OK otherwise; a later credit or fixture
removal does not re-arm that identity. The next UTC calendar period re-arms it. State is derived from the durable
marker, so caller-supplied NotificationState cannot suppress or repeat delivery. Changing a rule's identity creates
a separately eligible alert; changing subscribers or limits alone does not re-send an already fired rule. Pending
jobs keep the subscriber/message snapshot captured when the rule fired. Deleting a budget removes its markers and
jobs. Inactive budgets report OK.
AWS documents [once-per-period actual alerts](https://docs.aws.amazon.com/cost-management/latest/userguide/budgets-best-practices.html)
and describes [ALARM as a passed threshold](https://docs.aws.amazon.com/aws-cost-management/latest/APIReference/API_budgets_Notification.html).
The latched state after a credit is TinyStack's local rule; AWS's reference does not specify that recovery case.
AWS refreshes billing data at least daily; the local one-second fixture cadence is deliberate.

### Destinations and message shape

SNS subscribers publish through TinyStack SNS, using the topic ARN's account and Region, independently of the
budget client's signing Region. Only standard topics in the budget's account are supported. Missing, malformed,
FIFO and cross-account topics become recorded delivery failures. AWS also requires
[same-account topics and a Budgets topic policy](https://docs.aws.amazon.com/cost-management/latest/userguide/budgets-sns-policy.html).
TinyStack stores SNS policies but does not enforce service-principal authorization; keep portable policies in IaC.
SNS acceptance is a successful publish attempt, not proof that every subscription received the message. SNS logs
its own downstream outcomes separately.

The subject is `AWS Budgets: <budget name> has exceeded your alert threshold`, truncated to SNS's 100-byte limit
and stripped of CR/LF. The plain-text body contains the account, budget name/type, budgeted USD amount, ACTUAL
alert type, comparison and USD threshold, actual USD amount, UTC period and stable event ID. For example:

```text
AWS Budget Notification October 15, 2026
AWS Account 000000000000

Budget Name: monthly
Budget Type: Cost
Budgeted Amount: $100
Alert Type: ACTUAL
Alert Threshold: > $80.00000000000000
ACTUAL Amount: $81
Budget Period: 2026-10-01T00:00:00Z to 2026-11-01T00:00:00Z (UTC, end exclusive)
Event ID: <stable logical alert ID>

TinyStack fixture-backed budget alert.
```

This approximates the subject and fields shown in an
[AWS notification example](https://d1.awsstatic.com/events/Summits/cdmxsummit24/DEV207_10Errores_V2_CDMXSummit_20240708.pdf)
and the plain-text format in the
[AWS sample cost-event integration](https://github.com/aws-samples/sample-aws-tailored-cost-events-teams).
It is a local fixture alert, without AWS HTML styling, masked accounts, console/unsubscribe links or real mail.
Threshold text is formatted to 14 decimal places; comparisons retain full precision.

Direct EMAIL subscribers are captured without SES verification or sending mail. Inspect attempts at
`/_tinystack/deliveries` (`via: "budgets"`, budget ARN source, recipient/topic target, delivered/failed status and
error). Captured email payloads contain the ID, account, subject and body; SNS attempt payloads contain the plain
text. Emails also appear at `/_tinystack/messages?service=budgets&channel=email`. These recent inspectors retain
up to 256 delivery records in memory and clear on restart. Budget inventory exposes evaluation readiness,
calculated spend and notification state; notification reads expose the same current-period state.

### Persistence, retries and limits

The sent marker and subscriber delivery jobs commit together before any SNS call or email capture. Pending jobs
are claimed transactionally with a one-minute lease; an interrupted attempt becomes eligible after that lease.
There are at most **four attempts**, with **1, 2 and 4 seconds** between ordinary failures, rounded up to the next
one-second tick. All destination errors use this bounded local retry policy. Exhausted jobs retain their last
error in storage and leave the pending index. A crash on the final attempt leaves an unknown-outcome failure
after lease expiry. A process crash after publish/capture but before acknowledgment can cause duplicate delivery:
transport is **at-least-once within the retry limit**, while logical evaluation is once per notification/period.
Use the stable event ID to deduplicate consumers.

Persistent storage retains markers, pending jobs and final job outcomes across restarts; the recent delivery log
is separate and transient. Markers and completed/failed jobs remain until the budget is deleted or
`POST /_tinystack/reset` clears storage. Reset clears pending work, so it cannot be restored by an old completion;
a delivery already in flight may still reach its destination. Notification/subscriber deletion does not cancel
an already committed job. Existing limits remain five notifications per budget and ten EMAIL plus one SNS
subscriber per notification. No real billing data, forecasts, budget actions or cross-account delivery are added.
