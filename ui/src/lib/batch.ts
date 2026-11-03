import { api } from "./api";
import {
	allStatuses,
	type BatchScope,
	type ComputeEnvironment,
	type Job,
	type JobDefinition,
	type JobQueue,
	type JobSummary,
	sortDefinitions,
	sortJobs,
} from "./batchHelpers";

export const batchCall = <T = Record<string, unknown>>(
	s: BatchScope,
	op: string,
	input: Record<string, unknown> = {},
): Promise<T> => api.action("batch", op, s.account, input, s.region);

// batchList collects every page of a list operation; all of them take 100 at a time.
async function batchList<T>(
	s: BatchScope,
	op: string,
	key: string,
	input: Record<string, unknown> = {},
) {
	const items: T[] = [],
		seen = new Set<string>();
	let token: string | undefined;
	do {
		const out = await batchCall<Record<string, unknown>>(s, op, {
			...input,
			MaxResults: 100,
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

export const environments = (s: BatchScope) =>
	batchList<ComputeEnvironment>(
		s,
		"DescribeComputeEnvironments",
		"ComputeEnvironments",
	);
export const queues = (s: BatchScope) =>
	batchList<JobQueue>(s, "DescribeJobQueues", "JobQueues");

// definitions lists the revisions with a status, or all of them.
export async function definitions(s: BatchScope, status: string) {
	return sortDefinitions(
		await batchList<JobDefinition>(
			s,
			"DescribeJobDefinitions",
			"JobDefinitions",
			status === allStatuses ? {} : { Status: status },
		),
	);
}

// jobs lists a queue's jobs in one status. A job name filter ignores the
// status, and * matches every name, which lists the jobs in every status.
export async function jobs(s: BatchScope, queue: string, status: string) {
	return sortJobs(
		await batchList<JobSummary>(
			s,
			"ListJobs",
			"JobSummaryList",
			status === allStatuses
				? {
						JobQueue: queue,
						Filters: [{ Name: "JOB_NAME", Values: ["*"] }],
					}
				: { JobQueue: queue, JobStatus: status },
		),
	);
}

// childJobs lists the children of an array job in every status, by index.
export async function childJobs(s: BatchScope, arrayJob: string) {
	const children = await batchList<JobSummary>(
		s,
		"ListJobs",
		"JobSummaryList",
		{
			ArrayJobId: arrayJob,
		},
	);
	return children.sort(
		(a, b) => (a.ArrayProperties?.Index ?? 0) - (b.ArrayProperties?.Index ?? 0),
	);
}

export async function describeJob(s: BatchScope, id: string) {
	const out = await batchCall<{ Jobs?: Job[] }>(s, "DescribeJobs", {
		Jobs: [id],
	});
	const job = out.Jobs?.[0];
	if (!job) throw new Error(`Job ${id} was not found.`);
	return job;
}
