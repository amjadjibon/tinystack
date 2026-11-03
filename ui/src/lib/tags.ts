// Tag is the Key/Value pair most AWS APIs use for resource tags.
export type Tag = { Key?: string; Value?: string };

// tagList turns a tag map into the Key/Value list SDK inputs take.
export function tagList(tags: Record<string, string>) {
	return Object.entries(tags).map(([Key, Value]) => ({ Key, Value }));
}

// tagMap turns a Key/Value list from an SDK output into a tag map.
export function tagMap(tags?: Tag[]): Record<string, string> {
	return Object.fromEntries(
		(tags ?? []).map((t) => [t.Key ?? "", t.Value ?? ""]),
	);
}
