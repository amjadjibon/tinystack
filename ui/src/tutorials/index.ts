import { dynamodbFirstTable } from "./dynamodb-first-table.ts";
import { eventbridgeSqsRule } from "./eventbridge-sqs-rule.ts";
import { s3FirstBucket } from "./s3-first-bucket.ts";
import { snsSqsFanout } from "./sns-sqs-fanout.ts";
export const tutorials = [
	s3FirstBucket,
	snsSqsFanout,
	dynamodbFirstTable,
	eventbridgeSqsRule,
];
