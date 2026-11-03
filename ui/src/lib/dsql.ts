import { api } from "./api";
import { type ActionStep, errorText } from "./consoleActions";
import { containerPages } from "./containerHelpers";
import type { Cluster, DSQLScope } from "./dsqlHelpers";

export const dsqlCall = <T = Record<string, unknown>>(
	s: DSQLScope,
	op: string,
	input: Record<string, unknown> = {},
): Promise<T> => api.action("dsql", op, s.account, input, s.region);

// clusters reads every cluster in the scope, newest first. ListClusters returns
// only identifiers and ARNs, so each cluster is read for its status. One deleted
// since it was listed is left out.
export async function clusters(s: DSQLScope) {
	const listed = await containerPages<{ Identifier: string }>(
		(op, input) => dsqlCall(s, op, input),
		"ListClusters",
		"Clusters",
	);
	const rows = await Promise.all(
		listed.map((c) =>
			dsqlCall<Cluster>(s, "GetCluster", { Identifier: c.Identifier }).catch(
				(err) => {
					if (errorText(err).startsWith("ResourceNotFoundException"))
						return undefined;
					throw err;
				},
			),
		),
	);
	return rows
		.filter((c): c is Cluster => !!c)
		.sort(
			(a, b) =>
				(b.CreationTime ?? 0) - (a.CreationTime ?? 0) ||
				a.Identifier.localeCompare(b.Identifier),
		);
}

// tagPlan makes the calls that leave a cluster with the edited tags.
export const tagPlan =
	(s: DSQLScope, arn: string) =>
	(next: Record<string, string>, removed: string[]): ActionStep[] => [
		...(Object.keys(next).length
			? [
					{
						label: "TagResource",
						run: () =>
							dsqlCall(s, "TagResource", { ResourceArn: arn, Tags: next }),
					},
				]
			: []),
		...(removed.length
			? [
					{
						label: "UntagResource",
						run: () =>
							dsqlCall(s, "UntagResource", {
								ResourceArn: arn,
								TagKeys: removed,
							}),
					},
				]
			: []),
	];
