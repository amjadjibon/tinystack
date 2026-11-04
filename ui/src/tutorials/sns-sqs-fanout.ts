import { arn } from "./checks.ts";
import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const snsSqsFanout: Tutorial = {
	id: "sns-sqs-fanout",
	title: "Fan out messages with SNS and SQS",
	description:
		"Connect an SNS topic to an SQS queue. Publish a notification and receive the delivered message.",
	services: ["sns", "sqs"],
	duration: "10 min",
	prerequisites:
		"Requires SNS create/subscribe/publish and SQS create/receive/set-attributes permissions plus console reads. Cleanup requires SNS unsubscribe/delete-topic and SQS delete-queue. Under enforced authorization, allow sns.amazonaws.com sqs:SendMessage from this topic on the queue policy.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	cleanup: async (c, call) => {
		const topic = arn("sns", c.names.topic, c),
			queue = arn("sqs", c.names.queue, c);
		const subscriptions: string[] = [];
		let token: string | undefined;
		do {
			const page: {
				Subscriptions?: {
					SubscriptionArn: string;
					Protocol: string;
					Endpoint: string;
				}[];
				NextToken?: string;
			} = await call("sns", "ListSubscriptionsByTopic", {
				TopicArn: topic,
				...(token ? { NextToken: token } : {}),
			});
			for (const s of page.Subscriptions ?? [])
				if (s.Protocol === "sqs" && s.Endpoint === queue)
					subscriptions.push(s.SubscriptionArn);
			token = page.NextToken;
		} while (token);
		return [
			...subscriptions.map((id) => ({
				label: `Subscription ${id}`,
				run: () => call("sns", "Unsubscribe", { SubscriptionArn: id }),
			})),
			{
				label: `Topic ${topic} (including all its subscriptions)`,
				run: () => call("sns", "DeleteTopic", { TopicArn: topic }),
			},
			{
				label: `Queue ${queue} and its messages`,
				run: () =>
					call("sqs", "DeleteQueue", {
						QueueUrl: `/${c.account}/${c.names.queue}`,
					}),
			},
		];
	},
	tasks: [
		{
			title: "Connect a topic and queue",
			steps: [
				{
					title: "Create a topic",
					content:
						"Create a Standard topic named {topic}. Topics distribute a publication to every subscription.",
					command: "aws-local sns create-topic --name {topic}",
					hotspotId: HOTSPOT.snsCreate,
					route: "/services/sns",
					check: "topic",
				},
				{
					title: "Create a queue",
					content:
						"Create a Standard queue named {queue}. It retains messages until a consumer receives and deletes them.",
					command: "aws-local sqs create-queue --queue-name {queue}",
					hotspotId: HOTSPOT.sqsCreate,
					route: "/services/sqs",
					check: "queue",
				},
				{
					title: "Subscribe the queue",
					content:
						"Open {topic}, choose Create subscription, select SQS, and enter arn:aws:sqs:{region}:{account}:{queue}. In the queue Permissions tab, allow sns.amazonaws.com sqs:SendMessage with aws:SourceArn restricted to arn:aws:sns:{region}:{account}:{topic} when authorization is enforced.",
					command:
						"aws-local sns subscribe --topic-arn arn:aws:sns:{region}:{account}:{topic} --protocol sqs --notification-endpoint arn:aws:sqs:{region}:{account}:{queue}",
					hotspotId: HOTSPOT.snsSubscribe,
					route: "/services/sns?topic={topic}",
					check: "subscription",
				},
				{
					title: "Publish a notification",
					content:
						"Open the Publish message tab on {topic}, enter Hello TinyStack, and publish. This step waits for successful delivery to the generated queue.",
					command:
						"aws-local sns publish --topic-arn arn:aws:sns:{region}:{account}:{topic} --message 'Hello TinyStack'",
					hotspotId: HOTSPOT.snsPublish,
					route: "/services/sns?topic={topic}",
					check: "delivery",
				},
				{
					title: "Receive the message",
					content:
						"Open {queue}, select Send and receive messages, and Poll for messages. The SNS envelope contains your publication. Receiving hides the message for the visibility timeout.",
					command:
						"aws-local sqs receive-message --queue-url /{account}/{queue}",
					hotspotId: HOTSPOT.sqsReceive,
					route: "/services/sqs?queue={queue}",
					check: "receive",
				},
			],
		},
	],
};
