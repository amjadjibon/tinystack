import { api } from "./api";
import type { StreamScope } from "./streamingHelpers";
export function kinesisCall<T = Record<string, unknown>>(
	scope: StreamScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action(
		"kinesis",
		operation,
		scope.account,
		{ ...(scope.stream ? { StreamName: scope.stream } : {}), ...input },
		scope.region,
	);
}
