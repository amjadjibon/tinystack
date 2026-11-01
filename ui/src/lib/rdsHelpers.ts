export function validateName(name: string, max = 63) {
	if (
		!/^[a-zA-Z][a-zA-Z0-9-]*$/.test(name) ||
		name.length > max ||
		name.includes("--") ||
		name.endsWith("-")
	)
		throw new Error(
			`Identifier must start with a letter, contain only letters, numbers and hyphens, and have at most ${max} characters. Consecutive or trailing hyphens are forbidden.`,
		);
}
export function validatePassword(password: string) {
	if (
		password.length < 8 ||
		password.length > 41 ||
		/[^!-~]|[/@"]/.test(password)
	)
		throw new Error(
			"Password must contain 8–41 printable ASCII characters, excluding spaces, /, @ and double quotes.",
		);
}
export function finalSnapshotInput(skip: boolean, identifier: string) {
	if (skip) return { SkipFinalSnapshot: true };
	validateName(identifier, 255);
	return { SkipFinalSnapshot: false, FinalDBSnapshotIdentifier: identifier };
}

const settledSnapshotStatuses = new Set(["available", "failed", "deleted"]);

// hasPendingSnapshots reports whether any snapshot is still moving through a
// transitional status such as creating or copying.
export function hasPendingSnapshots(snapshots: { Status?: string }[]) {
	return snapshots.some(
		(s) => !settledSnapshotStatuses.has((s.Status ?? "").toLowerCase()),
	);
}
