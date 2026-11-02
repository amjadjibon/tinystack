import { api } from "./api";
export type CloudTrailScope = { account: string; region: string };
export function cloudtrailHref(s: CloudTrailScope) {
	return `/services/cloudtrail?${new URLSearchParams(s)}`;
}
export function cloudtrailCall<T = Record<string, unknown>>(
	s: CloudTrailScope,
	operation: string,
	input: Record<string, unknown> = {},
): Promise<T> {
	return api.action("cloudtrail", operation, s.account, input, s.region);
}
