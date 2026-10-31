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
