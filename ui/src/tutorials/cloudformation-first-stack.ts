import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const cloudformationFirstStack: Tutorial = {
	id: "cloudformation-first-stack",
	title: "Deploy a CloudFormation stack",
	description:
		"Create a stack containing an SQS queue, inspect its events and outputs, and update its tags.",
	services: ["cloudformation", "sqs"],
	duration: "10 min",
	prerequisites:
		"Requires CloudFormation create/describe/update and SQS resource permissions; cleanup requires DeleteStack and SQS DeleteQueue. Keep termination protection off and do not add unrelated resources to this generated stack.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	cleanup: async ({ names: n }, call) => [
		{
			label: `Stack ${n.stack} and its managed queue ${n.queue}`,
			run: () => call("cloudformation", "DeleteStack", { StackName: n.stack }),
		},
	],
	tasks: [
		{
			title: "Deploy a CloudFormation stack",
			steps: [
				{
					title: "Create a stack",
					content:
						"Choose Create stack and name it {stack}. Paste the TemplateBody JSON from the command into the console editor, validate it, and create directly without a change set. The template declares queue {queue} and a QueueUrl output.",
					command:
						'aws-local cloudformation create-stack --stack-name {stack} --template-body \'{"Resources":{"Queue":{"Type":"AWS::SQS::Queue","Properties":{"QueueName":"{queue}"}}},"Outputs":{"QueueUrl":{"Value":{"Ref":"Queue"}}}}\'',
					hotspotId: HOTSPOT.cfnCreate,
					route: "/services/cloudformation",
					check: "cfnStack",
				},
				{
					title: "Follow creation events",
					content:
						"Open {stack} and its Events tab. Refresh events until the stack reaches CREATE_COMPLETE. CloudFormation creates the declared queue before completing the stack.",
					command:
						"aws-local cloudformation describe-stack-events --stack-name {stack}",
					hotspotId: HOTSPOT.cfnEvents,
					route: "/services/cloudformation?stack={stack}",
					check: "cfnCreated",
				},
				{
					title: "View the queue output",
					content:
						"Select Outputs and inspect QueueUrl. Outputs expose useful resource values; Ref on an SQS queue returns its URL. The check uses the DescribeStacks response that the detail page also loads.",
					command:
						"aws-local cloudformation describe-stacks --stack-name {stack}",
					hotspotId: HOTSPOT.cfnOutputs,
					route: "/services/cloudformation?stack={stack}",
					check: "cfnOutputs",
				},
				{
					title: "Update the stack",
					content:
						"Choose Update stack, keep Use previous template, validate it, and add tag tutorial=updated. Leave Create change set off and submit. Follow Events to UPDATE_COMPLETE. Tags let you update stack metadata without replacing the queue.",
					command:
						"aws-local cloudformation update-stack --stack-name {stack} --use-previous-template --tags Key=tutorial,Value=updated",
					hotspotId: HOTSPOT.cfnUpdate,
					route: "/services/cloudformation?stack={stack}",
					check: "cfnUpdated",
				},
			],
		},
	],
};
