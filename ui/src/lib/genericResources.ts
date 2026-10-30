import type { api, Resource } from "./api";
import type { ActionStep } from "./consoleActions";

export type ResourceDelete = {
	service: string;
	operation: string;
	field: string;
	identifier: "name" | "arn" | "arn-suffix";
};

export function deleteInput(resource: Resource, action: ResourceDelete) {
	if (resource.service !== action.service)
		throw new Error("Delete capability does not match the resource service.");
	let id: string;
	switch (action.identifier) {
		case "name":
			id = resource.name;
			break;
		case "arn":
			id = resource.arn;
			break;
		case "arn-suffix":
			id = resource.arn.includes("/")
				? (resource.arn.split("/").at(-1) ?? "")
				: "";
			break;
		default:
			throw new Error("Unsupported resource identifier.");
	}
	if (!id) throw new Error("The resource has no delete identifier.");
	return { [action.field]: id };
}

export function tagChanges(
	current: Record<string, string>,
	next: Record<string, string>,
) {
	if (Object.keys(next).length > 50)
		throw new Error("A resource can have at most 50 tags.");
	for (const [key, value] of Object.entries(next)) {
		if ([...key].length < 1 || [...key].length > 128)
			throw new Error("Tag keys must be 1 to 128 characters long.");
		if ([...value].length > 256)
			throw new Error("Tag values must be at most 256 characters long.");
		if (key.toLowerCase().startsWith("aws:"))
			throw new Error("Tag keys cannot start with the reserved prefix aws:.");
	}
	return {
		removed: Object.keys(current).filter((key) => !Object.hasOwn(next, key)),
		added: Object.fromEntries(
			Object.entries(next).filter(([key, value]) => current[key] !== value),
		),
	};
}

export function taggingFailure(output: {
	FailedResourcesMap?: Record<
		string,
		{ ErrorCode?: string; ErrorMessage?: string }
	>;
}) {
	const failures = Object.entries(output.FailedResourcesMap ?? {});
	if (failures.length)
		throw new Error(
			failures
				.map(
					([arn, error]) => `${arn}: ${error.ErrorCode}: ${error.ErrorMessage}`,
				)
				.join("\n"),
		);
}

export function resourceTagPlan(
	call: typeof api.action,
	resource: Resource,
	region: string,
	next: Record<string, string>,
): ActionStep[] {
	const { added, removed } = tagChanges(resource.tags ?? {}, next);
	const run = async (operation: string, input: Record<string, unknown>) => {
		const output = await call<Parameters<typeof taggingFailure>[0]>(
			"resourcegroupstaggingapi",
			operation,
			resource.account,
			{ ResourceARNList: [resource.arn], ...input },
			region,
		);
		taggingFailure(output);
	};
	return [
		...(removed.length
			? [
					{
						label: "Remove tags",
						run: () => run("UntagResources", { TagKeys: removed }),
					},
				]
			: []),
		...(Object.keys(added).length
			? [{ label: "Set tags", run: () => run("TagResources", { Tags: added }) }]
			: []),
	];
}
