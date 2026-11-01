import { api } from "./api";
import type { StreamScope } from "./streamingHelpers";
export function firehoseCall<T = Record<string, unknown>>(
	scope: StreamScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action(
		"firehose",
		operation,
		scope.account,
		{ ...(scope.stream ? { DeliveryStreamName: scope.stream } : {}), ...input },
		scope.region,
	);
}
