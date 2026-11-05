import { arn } from "./checks.ts";
import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const sfnFirstStateMachine: Tutorial = {
	id: "sfn-first-state-machine",
	title: "Run a Step Functions workflow",
	description:
		"Create a Pass/Choice workflow, start it, and inspect its succeeded output. No other service tasks are needed.",
	services: ["sfn"],
	duration: "5 min",
	prerequisites:
		"Requires states:CreateStateMachine, StartExecution, DescribeExecution, console listing and DeleteStateMachine permissions. Use Standard type with logging off and role arn:aws:iam::{account}:role/states (or an existing execution role when required by authorization). Pass and Choice states call no other services.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	tasks: [
		{
			title: "Run a Step Functions workflow",
			steps: [
				{
					title: "Create a state machine",
					content:
						'Choose Create state machine, name it {machine}, keep Standard and logging off. Set the execution role ARN and paste this definition: {"StartAt": "Hello", "States": {"Hello": {"Type": "Pass", "Result": {"message": "Hello TinyStack"}, "Next": "Choose"}, "Choose": {"Type": "Choice", "Choices": [{"Variable": "$.message", "StringEquals": "Hello TinyStack", "Next": "Done"}], "Default": "Done"}, "Done": {"Type": "Pass", "End": true}}}. Pass creates a message, Choice tests it, and the final Pass returns it unchanged.',
					command:
						'aws-local stepfunctions create-state-machine --name {machine} --type STANDARD --role-arn arn:aws:iam::{account}:role/states --definition \'{"StartAt": "Hello", "States": {"Hello": {"Type": "Pass", "Result": {"message": "Hello TinyStack"}, "Next": "Choose"}, "Choose": {"Type": "Choice", "Choices": [{"Variable": "$.message", "StringEquals": "Hello TinyStack", "Next": "Done"}], "Default": "Done"}, "Done": {"Type": "Pass", "End": true}}}\'',
					hotspotId: HOTSPOT.sfnCreate,
					route: "/services/sfn",
					check: "stateMachine",
				},
				{
					title: "Start an execution",
					content:
						"Open {machine}, choose Start execution, replace the execution name with {execution}, keep input {}, and confirm. Starting returns an execution ARN. The live check confirms this named execution exists; completion is checked separately.",
					command:
						"aws-local stepfunctions start-execution --state-machine-arn arn:aws:states:{region}:{account}:stateMachine:{machine} --name {execution} --input '{}'",
					hotspotId: HOTSPOT.sfnStart,
					route: "/services/sfn?machine={machine}",
					check: "executionStarted",
				},
				{
					title: "Inspect the succeeded output",
					content:
						'Open execution {execution} and choose Refresh execution. Confirm SUCCEEDED and output {"message":"Hello TinyStack"}. Pass/Choice transitions are visible in Execution history. This check waits for this generated execution to reach SUCCEEDED.',
					command:
						"aws-local stepfunctions describe-execution --execution-arn arn:aws:states:{region}:{account}:execution:{machine}:{execution}",
					hotspotId: HOTSPOT.sfnInspect,
					route: "/services/sfn?machine={machine}&execution={execution}",
					check: "executionSucceeded",
				},
			],
		},
	],
	cleanup: async (c, call) => [
		{
			label: `State machine ${c.names.machine} and its executions`,
			run: () =>
				call("sfn", "DeleteStateMachine", {
					StateMachineArn: arn("states", `stateMachine:${c.names.machine}`, c),
				}),
		},
	],
};
