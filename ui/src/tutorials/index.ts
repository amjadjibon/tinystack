import { dynamodbFirstTable } from "./dynamodb-first-table.ts";
import { eventbridgeSqsRule } from "./eventbridge-sqs-rule.ts";
import { lambdaFirstFunction } from "./lambda-first-function.ts";
import { s3FirstBucket } from "./s3-first-bucket.ts";
import { secretsAndParameters } from "./secrets-and-parameters.ts";
import { sfnFirstStateMachine } from "./sfn-first-state-machine.ts";
import { snsSqsFanout } from "./sns-sqs-fanout.ts";
export const tutorials = [
	s3FirstBucket,
	snsSqsFanout,
	dynamodbFirstTable,
	eventbridgeSqsRule,
	lambdaFirstFunction,
	sfnFirstStateMachine,
	secretsAndParameters,
];
