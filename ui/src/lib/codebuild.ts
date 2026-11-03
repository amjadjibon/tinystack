import { api } from "./api";
import type { Build, Project, Scope } from "./codebuildHelpers";
export const codebuildCall = <T = Record<string, unknown>>(
	s: Scope,
	op: string,
	input: Record<string, unknown> = {},
): Promise<T> => api.action("codebuild", op, s.account, input, s.region);
export async function list<T>(
	s: Scope,
	op: string,
	key: string,
	input: Record<string, unknown> = {},
) {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await codebuildCall<Record<string, unknown>>(s, op, {
			...input,
			...(token ? { NextToken: token } : {}),
		});
		items.push(...((out[key] ?? []) as T[]));
		token = out.NextToken as string | undefined;
		if (token && seen.has(token))
			throw new Error("The server returned a repeated page token.");
		if (token) seen.add(token);
	} while (token);
	return items;
}
export async function batch<T>(
	s: Scope,
	op: string,
	key: string,
	inputKey: string,
	ids: string[],
) {
	const chunks: Promise<T[]>[] = [];
	for (let i = 0; i < ids.length; i += 100)
		chunks.push(
			codebuildCall<Record<string, T[]>>(s, op, {
				[inputKey]: ids.slice(i, i + 100),
			}).then((out) => out[key] ?? []),
		);
	return (await Promise.all(chunks)).flat();
}
export async function projects(s: Scope) {
	return batch<Project>(
		s,
		"BatchGetProjects",
		"Projects",
		"Names",
		await list<string>(s, "ListProjects", "Projects"),
	);
}
export async function builds(s: Scope, project = "") {
	return batch<Build>(
		s,
		"BatchGetBuilds",
		"Builds",
		"Ids",
		await list<string>(
			s,
			project ? "ListBuildsForProject" : "ListBuilds",
			"Ids",
			project ? { ProjectName: project } : {},
		),
	);
}
export async function deleteBuilds(s: Scope, ids: string[]) {
	for (let i = 0; i < ids.length; i += 100) {
		const out = await codebuildCall<{
			BuildsNotDeleted?: { Id: string; StatusCode: string }[];
		}>(s, "BatchDeleteBuilds", { Ids: ids.slice(i, i + 100) });
		if (out.BuildsNotDeleted?.length)
			throw new Error(
				out.BuildsNotDeleted.map((b) => `${b.Id}: ${b.StatusCode}`).join("; "),
			);
	}
}
