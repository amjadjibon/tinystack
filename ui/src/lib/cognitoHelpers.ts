export type Attribute = { Name: string; Value: string };
export type CognitoUser = {
	Username: string;
	Attributes?: Attribute[];
	UserStatus?: string;
	Enabled?: boolean;
};
export function matchesUser(user: CognitoUser, query: string) {
	const needle = query.trim().toLowerCase();
	return [
		user.Username,
		user.UserStatus ?? "",
		...(user.Attributes ?? [])
			.filter((a) => a.Name === "email")
			.map((a) => a.Value),
	].some((v) => v.toLowerCase().includes(needle));
}
export function changedAttributes(
	before: Attribute[],
	values: Record<string, string>,
): Attribute[] {
	return Object.entries(values)
		.filter(
			([name, value]) => before.find((a) => a.Name === name)?.Value !== value,
		)
		.map(([Name, Value]) => ({ Name, Value }));
}
