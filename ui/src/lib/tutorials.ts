import { adminRequest } from "#/lib/adminApi";
import {
	type Check,
	emptySnapshot,
	type Snapshot,
	sources,
} from "../tutorials/checks";
export async function fetchSnapshot(kind?: Check): Promise<Snapshot> {
	const snapshot = emptySnapshot();
	if (!kind) return snapshot;
	await Promise.all(
		sources[kind].map(async (source) => {
			const body = await adminRequest<Record<string, unknown[]>>(
				`/_tinystack/${source}`,
			);
			Object.assign(snapshot, { [source]: body[source] ?? [] });
		}),
	);
	return snapshot;
}
