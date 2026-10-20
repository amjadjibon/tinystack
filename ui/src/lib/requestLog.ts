import type { RequestRecord } from "./api.ts";

// Delivery context is already stored in the inspector's request body.
export function deliveryContext(
	record: RequestRecord,
): Record<string, string> | undefined {
	if (record.method !== "DELIVER") return undefined;
	try {
		const body: unknown = JSON.parse(record.request_body);
		if (!body || typeof body !== "object" || Array.isArray(body)) return {};
		return Object.fromEntries(
			Object.entries(body).filter((entry) => typeof entry[1] === "string"),
		);
	} catch {
		return {};
	}
}

export function isDenial(record: RequestRecord): boolean {
	return (
		record.authorization?.allowed === false ||
		record.status === 401 ||
		record.status === 403
	);
}

export function causedDeliveries(
	records: RequestRecord[],
	requestID: string,
): RequestRecord[] {
	return records.filter(
		(record) => deliveryContext(record)?.caused_by === requestID,
	);
}
