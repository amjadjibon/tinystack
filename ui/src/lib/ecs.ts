import { api } from "./api";
import type { ContainerScope } from "./ecr";
export function ecsCall<T = Record<string, unknown>>(
	s: ContainerScope,
	operation: string,
	input: Record<string, unknown> = {},
) {
	return api.action<T>("ecs", operation, s.account, input, s.region);
}
export { desiredCount, taskDefinitionInput } from "./containerHelpers";
export type Cluster = {
	ClusterArn: string;
	ClusterName: string;
	Status: string;
};
export type ECSService = {
	ServiceArn: string;
	ServiceName: string;
	Status: string;
	DesiredCount: number;
	RunningCount: number;
	TaskDefinition: string;
};
export type ECSTask = {
	TaskArn: string;
	TaskDefinitionArn: string;
	LastStatus: string;
	DesiredStatus: string;
	StoppedReason?: string;
};
export async function ecsDescribe<T>(
	s: ContainerScope,
	operation: string,
	key: string,
	inputKey: string,
	arns: string[],
	input: Record<string, unknown> = {},
	batch = 100,
) {
	const items: T[] = [];
	for (let i = 0; i < arns.length; i += batch) {
		const out = await ecsCall(s, operation, {
			...input,
			[inputKey]: arns.slice(i, i + batch),
		});
		if ((out.Failures as unknown[] | undefined)?.length)
			throw new Error(JSON.stringify(out.Failures));
		items.push(...((out[key] as T[] | undefined) ?? []));
	}
	return items;
}
