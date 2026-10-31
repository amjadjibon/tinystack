export const HOTSPOT = {
	s3Create: "s3.create-bucket",
	s3Upload: "s3.upload",
	s3Download: "s3.download",
	snsCreate: "sns.create-topic",
	sqsCreate: "sqs.create-queue",
	snsSubscribe: "sns.subscribe",
	snsPublish: "sns.publish",
	sqsReceive: "sqs.receive",
} as const;
export type HotspotId = (typeof HOTSPOT)[keyof typeof HOTSPOT];
