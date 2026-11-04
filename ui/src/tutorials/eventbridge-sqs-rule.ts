import { arn } from "./checks.ts";
import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const eventbridgeSqsRule: Tutorial = {
	id: "eventbridge-sqs-rule",
	title: "Route events to SQS with EventBridge",
	description:
		"Match events on the default bus with a rule, deliver them to an SQS queue, and receive the event.",
	services: ["eventbridge", "sqs"],
	duration: "10 min",
	prerequisites:
		"Requires EventBridge put-rule/put-targets/put-events and SQS create/receive/set-attributes permissions plus console reads. Cleanup requires EventBridge list-targets/remove-targets/delete-rule and SQS delete-queue. Under enforced authorization, allow events.amazonaws.com sqs:SendMessage from this rule on the queue policy.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	cleanup: async (c, call) => {
		const n = c.names,
			queue = arn("sqs", n.queue, c);
		const ids: string[] = [];
		let token: string | undefined;
		do {
			const page: {
				Targets?: { Id: string; Arn: string }[];
				NextToken?: string;
			} = await call("eventbridge", "ListTargetsByRule", {
				Rule: n.rule,
				...(token ? { NextToken: token } : {}),
			});
			for (const t of page.Targets ?? []) ids.push(t.Id);
			token = page.NextToken;
		} while (token);
		return [
			...(ids.length
				? [
						{
							label: `${ids.length > 1 ? "Targets" : "Target"} ${ids.join(", ")} on rule ${n.rule}`,
							run: () =>
								call("eventbridge", "RemoveTargets", {
									Rule: n.rule,
									Ids: ids,
								}),
						},
					]
				: []),
			{
				label: `Rule ${arn("events", `rule/${n.rule}`, c)}`,
				run: () => call("eventbridge", "DeleteRule", { Name: n.rule }),
			},
			{
				label: `Queue ${queue} and its messages`,
				run: () =>
					call("sqs", "DeleteQueue", { QueueUrl: `/${c.account}/${n.queue}` }),
			},
		];
	},
	tasks: [
		{
			title: "Route an event to a queue",
			steps: [
				{
					title: "Create a queue",
					content:
						"Create a Standard queue named {queue}. It holds the matched events until you receive them.",
					command: "aws-local sqs create-queue --queue-name {queue}",
					hotspotId: HOTSPOT.sqsCreate,
					route: "/services/sqs",
					check: "queue",
				},
				{
					title: "Create a rule",
					content:
						'Choose Create rule on the default bus, name it {rule}, and keep the event pattern {"source":["my.app"]}. A rule matches events by their fields; this one matches every event whose source is my.app.',
					command:
						'aws-local events put-rule --name {rule} --event-pattern \'{"source":["my.app"]}\'',
					hotspotId: HOTSPOT.eventsCreateRule,
					route: "/services/eventbridge?bus=default&section=rules",
					check: "rule",
				},
				{
					title: "Target the queue",
					content:
						"Open {rule}, choose Add target, and enter arn:aws:sqs:{region}:{account}:{queue}. In the queue Permissions tab, allow events.amazonaws.com sqs:SendMessage with aws:SourceArn restricted to arn:aws:events:{region}:{account}:rule/{rule} when authorization is enforced.",
					command:
						"aws-local events put-targets --rule {rule} --targets Id=queue,Arn=arn:aws:sqs:{region}:{account}:{queue}",
					hotspotId: HOTSPOT.eventsAddTarget,
					route:
						"/services/eventbridge?bus=default&rule={rule}&section=rules&tab=targets",
					check: "target",
				},
				{
					title: "Send an event",
					content:
						"Choose Send event, keep source my.app, and send. This step waits for the rule to deliver the event to {queue}, not just for the bus to accept it.",
					command:
						'aws-local events put-events --entries \'[{"Source":"my.app","DetailType":"Tutorial","Detail":"{}"}]\'',
					hotspotId: HOTSPOT.eventsSend,
					route: "/services/eventbridge?bus=default&section=rules",
					check: "event",
				},
				{
					title: "Receive the event",
					content:
						"Open {queue}, select Send and receive messages, and Poll for messages. The message body is the full event, with source, detail-type and detail.",
					command:
						"aws-local sqs receive-message --queue-url /{account}/{queue}",
					hotspotId: HOTSPOT.sqsReceive,
					route: "/services/sqs?queue={queue}&tab=messages",
					check: "receive",
				},
			],
		},
	],
};
