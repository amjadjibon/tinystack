export type StreamScope = { account: string; region: string; stream?: string };
export type StreamService = "kinesis" | "firehose";
export type Shard = {
	ShardId: string;
	HashKeyRange?: { StartingHashKey: string; EndingHashKey: string };
	SequenceNumberRange?: {
		StartingSequenceNumber: string;
		EndingSequenceNumber?: string;
	};
};
export type Destination = {
	DestinationId: string;
	S3DestinationDescription?: S3Destination;
	ExtendedS3DestinationDescription?: S3Destination;
};
export type S3Destination = {
	BucketARN: string;
	RoleARN: string;
	Prefix?: string;
	BufferingHints?: { SizeInMBs?: number; IntervalInSeconds?: number };
	CompressionFormat?: string;
};
export type StreamDescription = {
	StreamName?: string;
	StreamARN?: string;
	StreamStatus?: string;
	StreamModeDetails?: { StreamMode: string };
	RetentionPeriodHours?: number;
	EncryptionType?: string;
	KeyId?: string;
	EnhancedMonitoring?: { ShardLevelMetrics?: string[] }[];
	DeliveryStreamName?: string;
	DeliveryStreamARN?: string;
	DeliveryStreamStatus?: string;
	DeliveryStreamType?: string;
	VersionId?: string;
	Destinations?: Destination[];
	Source?: unknown;
};
export function streamHref(service: StreamService, scope: StreamScope) {
	const params = new URLSearchParams({
		account: scope.account,
		region: scope.region,
	});
	if (scope.stream) params.set("stream", scope.stream);
	return `/services/${service}?${params}`;
}
export function encodeRecord(text: string) {
	const bytes = new TextEncoder().encode(text);
	if (!bytes.length) throw new Error("Enter record data.");
	return btoa(Array.from(bytes, (b) => String.fromCharCode(b)).join(""));
}
export function decodeRecord(data: string) {
	try {
		return new TextDecoder("utf-8", { fatal: true }).decode(
			Uint8Array.from(atob(data), (c) => c.charCodeAt(0)),
		);
	} catch {
		return `[Binary data, base64] ${data}`;
	}
}
export type StreamCall = <T = Record<string, unknown>>(
	operation: string,
	input?: Record<string, unknown>,
) => Promise<T>;
export async function streamNames(
	call: StreamCall,
	service: StreamService,
): Promise<string[]> {
	const names: string[] = [];
	const seen = new Set<string>();
	let cursor: string | undefined;
	do {
		const out = await call<{
			StreamNames?: string[];
			DeliveryStreamNames?: string[];
			HasMoreStreams?: boolean;
			HasMoreDeliveryStreams?: boolean;
		}>(service === "kinesis" ? "ListStreams" : "ListDeliveryStreams", {
			Limit: 100,
			...(cursor
				? {
						[service === "kinesis"
							? "ExclusiveStartStreamName"
							: "ExclusiveStartDeliveryStreamName"]: cursor,
					}
				: {}),
		});
		const page = out.StreamNames ?? out.DeliveryStreamNames ?? [];
		names.push(...page);
		if (!(out.HasMoreStreams || out.HasMoreDeliveryStreams)) break;
		cursor = page.at(-1);
		if (!cursor || seen.has(cursor))
			throw new Error("The server returned an invalid stream page.");
		seen.add(cursor);
	} while (cursor);
	return names;
}
export async function streamTags(call: StreamCall, service: StreamService) {
	const tags = new Map<string, string>();
	let cursor: string | undefined;
	const seen = new Set<string>();
	do {
		const out = await call<{
			Tags?: { Key: string; Value: string }[];
			HasMoreTags?: boolean;
		}>(
			service === "kinesis" ? "ListTagsForStream" : "ListTagsForDeliveryStream",
			{ Limit: 50, ...(cursor ? { ExclusiveStartTagKey: cursor } : {}) },
		);
		for (const tag of out.Tags ?? []) tags.set(tag.Key, tag.Value);
		if (!out.HasMoreTags) break;
		cursor = out.Tags?.at(-1)?.Key;
		if (!cursor || seen.has(cursor))
			throw new Error("The server returned an invalid tag page.");
		seen.add(cursor);
	} while (cursor);
	return Object.fromEntries(tags);
}
export async function streamShards(call: StreamCall) {
	const shards: Shard[] = [];
	let token: string | undefined;
	const seen = new Set<string>();
	do {
		const out = await call<{ Shards?: Shard[]; NextToken?: string }>(
			"ListShards",
			token ? { StreamName: undefined, NextToken: token } : { MaxResults: 100 },
		);
		shards.push(...(out.Shards ?? []));
		token = out.NextToken;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated shard token.");
		if (token) seen.add(token);
	} while (token);
	return shards;
}
