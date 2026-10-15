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
supported budget. There is no ForecastedSpend, automatic price calculation, notification evaluation or delivery.
CalculatedSpend is read-only and rejected on budget writes.
