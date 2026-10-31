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
	cleanup: ["subscription", "topic", "queue"],
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
