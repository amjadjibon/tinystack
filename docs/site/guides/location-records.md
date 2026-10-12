# Amazon Location tracker records

TinyStack persists tracker configuration and tags. Inventory reports `RecordsOnly=true` and
`ExecutionReady=false`. PositionFiltering records a configuration choice; TinyStack does not ingest,
filter, store, evaluate or return device positions, associate consumers or send location events.

The upstream model is pinned to AWS SDK Go v2 commit
`2ba0e39015ddf9c91c6c378f8a4fb79dcb4353da`, with SHA256
`b6afe8e89a91ebf6cedb12595cadaa8168aa08483160017904d8b44d3daa8d89`.
The local service key, CLI command and Go SDK package are `location`; SigV4 signing is `geo`.
Tracker API endpoint prefixes are `cp.tracking` and metadata/tag prefixes are `cp.metadata`,
over the regional `geo` endpoint. Local clients use one TinyStack endpoint override for both. The Go SDK also needs its operation
host prefix disabled for local endpoints (BaseEndpoint alone is insufficient):

```go
client := location.New(location.Options{
    Region: "us-east-1", BaseEndpoint: aws.String("http://127.0.0.1:4566"),
    Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
    APIOptions: []func(*middleware.Stack) error{func(stack *middleware.Stack) error {
        return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("DisableHostPrefix",
            func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
                middleware.InitializeOutput, middleware.Metadata, error,
            ) {
                return next.HandleInitialize(smithyhttp.DisableEndpointHostPrefix(ctx, true), in)
            }), middleware.Before)
    }},
})
```

Here `location` is the AWS SDK service package, `middleware` is Smithy middleware and `smithyhttp`
is Smithy transport/http. For AWS CLI/botocore, set `AWS_DISABLE_HOST_PREFIX_INJECTION=true` as in the lifecycle below.
The lifecycle was verified with AWS CLI 1.42.73 / botocore 1.40.73.
Terraform's provider endpoint key is `location`.

## Complete selected API surface

All eight operations use the pinned REST JSON protocol. Only these members are selected:

| Operation | Input | Output |
| --- | --- | --- |
| CreateTracker | TrackerName, Description, PositionFiltering, Tags | TrackerName, TrackerArn, CreateTime |
| DescribeTracker | TrackerName (HTTP label) | TrackerName, TrackerArn, Description, PositionFiltering, Tags, CreateTime, UpdateTime |
| ListTrackers | MaxResults, NextToken (JSON body) | Entries, NextToken; each entry contains TrackerName, Description, CreateTime, UpdateTime |
| UpdateTracker | TrackerName (HTTP label), Description, PositionFiltering | TrackerName, TrackerArn, UpdateTime |
| DeleteTracker | TrackerName (HTTP label) | Empty modeled structure |
| ListTagsForResource | ResourceArn (HTTP label) | Tags |
| TagResource | ResourceArn (HTTP label), Tags | Empty modeled structure |
| UntagResource | ResourceArn (HTTP label), TagKeys (repeated `tagKeys` query parameter) | Empty modeled structure |

ListTrackers has no modeled filters. No unselected filter is supported. No ClientToken is modeled for
these operations, and none is invented. Duplicate CreateTracker returns ConflictException; a repeated
delete returns ResourceNotFoundException. The model's idempotent operation trait does not introduce a token.

## Configuration, ownership and time

TrackerName requires 1–100 ASCII letters, digits, hyphens, periods or underscores, with no spaces.
Names are unique within the caller's account and Region. Only commercial tracker ARNs are admitted:
`arn:aws:geo:REGION:ACCOUNT:tracker/NAME`. Tag operations require that exact resource kind and caller scope;
foreign, malformed and alternate service ARNs reject before accessing metadata.

Description is a string of 0–1000 Unicode characters, defaulting locally to an empty string.
PositionFiltering is exactly `TimeBased`, `DistanceBased` or `AccuracyBased`, defaulting to `TimeBased`
according to the [CreateTracker API](https://docs.aws.amazon.com/location/previous/APIReference/API_CreateTracker.html)
and [tracker creation guide](https://docs.aws.amazon.com/location/latest/developerguide/start-create-tracker.html).
All three are metadata choices only.

[UpdateTracker](https://docs.aws.amazon.com/location/previous/APIReference/API_UpdateTracker.html) merges supplied
selected fields into the complete live configuration in one storage transaction. Omitted fields retain
live values; Description `""` clears it; empty/unknown PositionFiltering rejects. Invalid candidates,
including final tag overflow, preserve the entire record and both timestamps.

CreateTime and UpdateTime are actual UTC modeled timestamps. Creation sets both to the same time.
Locally, effective configuration or tag changes advance UpdateTime; no-op updates, same-value tags and
removal of absent tags retain it. CreateTime and the private incarnation remain stable across updates,
SQLite stop/reopen, offline fixture export/import and restart. Delete/recreate has fresh creation metadata,
tags and incarnation even though the name-based ARN repeats. Inputs and returned data have independent ownership.
No runtime status, allocated endpoint, event delivery or encryption result is fabricated.

## Tags, pagination and errors

CreateTracker accepts omitted or empty Tags. TagResource requires Tags; an empty map is a local no-op on
an existing tracker. Tags merge, replace same-key values and admit at most 50 final keys. Keys require
1–128 Unicode characters and values 0–256. The pinned character pattern permits Unicode letters,
numbers and separators plus `_ . , : / = + - @`. Empty values are allowed. Locally, keys starting with
`aws:` in any case are reserved. UntagResource takes 1–50 valid keys, tolerates repeated or absent keys
and removes them atomically. Overflow never partially merges tags. Concurrent creation has one winner;
disjoint configuration and tag transactions preserve each other's committed fields.
Resource Groups Tagging API dispatches real location TagResource/UntagResource calls for `geo` tracker ARNs.

[ListTrackers](https://docs.aws.amazon.com/location/previous/APIReference/API_ListTrackers.html) accepts
MaxResults 1–100, default 100. Entries sort by name. NextToken is an opaque deterministic cursor bound
to account, Region, operation and the cursor's tracker incarnation. Malformed/foreign tokens and tokens
whose cursor was recreated reject. Deleted cursors still resume strictly after their name. Page sizes
may change. This is a live cursor, not a frozen snapshot; later additions can appear.
ListTagsForResource returns the complete map and has no pagination members.

Modeled service errors use `message`: ValidationException HTTP 400, ConflictException HTTP 409 and
ResourceNotFoundException HTTP 404. Local configuration errors also include modeled `reason` and
`fieldList`. The shared protocol rejects unselected fields, including false/null/empty values,
with an unsupported-operation error; malformed JSON/types use serialization errors.
Signature checks use the shared identity verification mode and the actual `geo` signing name.

## CloudFormation

The literal type [AWS::Location::Tracker](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-location-tracker.html)
supports required TrackerName and optional Description, PositionFiltering and Tags.
Unknown properties reject, including KmsKeyId, EventBridgeEnabled, KmsKeyEnableGeospatialQueries,
PricingPlan and PricingPlanDataSource. Null selected properties reject. Each tag requires exactly string
Key and Value, unique keys, and the same 50-key final limit; explicit empty Tags is allowed.
CF additionally applies its [documented tag character patterns](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-properties-location-tracker-tag.html):
Key `^[a-zA-Z+-=._:/]+$` and Value `^[A-Za-z0-9 _=@:.+-/]*$`, intersected with the selected API rules.
Direct API tags retain their wider Unicode patterns.

Ref returns the tracker name. GetAtt supports only Arn, TrackerArn (the same ARN), CreateTime and UpdateTime.
TrackerName changes replace; selected configuration and tags update in place. Removing PositionFiltering
restores TimeBased. Removing Tags deletes prior stack-owned keys and retains external keys. Description
removal is explicitly unsupported; use `""` to clear it. Removing TrackerName fails. Required/unsupported
changes and removals are validated before writes, including when combined with a rename.

Config and the final owned/external tag set commit atomically. Replacing two disjoint sets of 50 owned keys
does not transiently overflow; a final set exceeding 50 rejects config, timestamps and tags together.
A later-resource failure compensates using the actual live pre-update config/tags/identity/CreateTime,
including external API changes. Local compensation restores the prior public UpdateTime exactly and
writes a fresh private revision guard. A concurrent write or recreated incarnation prevents stale
compensation. Successful name replacement cleans the retired record; collision or later failure preserves
original and unrelated records. Failed creation rollback removes records created by that attempt.

## Executable CLI lifecycle

Start TinyStack with isolated storage and run this block. Adjust only the endpoint when choosing a
separate port. Use `AWS_PAGER=""` to avoid an interactive pager.

```sh
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
export AWS_ENDPOINT_URL=http://127.0.0.1:4566 AWS_PAGER=""
export AWS_DISABLE_HOST_PREFIX_INJECTION=true
aws location create-tracker --tracker-name CLITracker --description 'Local records only' --position-filtering DistanceBased --tags owner=cli
TRACKER_ARN=$(aws location describe-tracker --tracker-name CLITracker --query TrackerArn --output text)
aws location list-trackers --max-results 1
aws location update-tracker --tracker-name CLITracker --description '' --position-filtering AccuracyBased
aws location describe-tracker --tracker-name CLITracker
aws location tag-resource --resource-arn "$TRACKER_ARN" --tags stage=demo
aws location list-tags-for-resource --resource-arn "$TRACKER_ARN"
aws location untag-resource --resource-arn "$TRACKER_ARN" --tag-keys stage
aws location list-tags-for-resource --resource-arn "$TRACKER_ARN"
aws location delete-tracker --tracker-name CLITracker
aws location list-trackers
```

## Remaining scope

Device positions and history, consumer associations, geofences, events, KMS/IAM behavior, credentials,
networking, execution readiness, maps, search, places and routes remain unsupported. BatchUpdateDevicePosition,
GetDevicePosition, BatchGetDevicePosition, GetDevicePositionHistory and consumer/runtime routes cannot succeed.
PricingPlan and PricingPlanDataSource are excluded on input and output. The current API only admits
RequestBasedUsage when explicitly supplying deprecated PricingPlan; this implementation does not fabricate
billing metadata or accept old mobile-asset plans. `RequestBasedUsage` is a pricing value, not an operation.
See the explicit [Location runtime and broader records roadmap](../roadmap/services/location-runtime.md),
[service catalogue](../services/location.md), [persistence](persistence.md) and
[CloudFormation support](cloudformation-resources.md).
