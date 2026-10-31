import type { Resource } from "./api";

export type DriftInformation = { StackDriftStatus?: string };
export type StackSummary = {
	StackId: string;
	StackName: string;
	StackStatus: string;
	StackStatusReason?: string;
	TemplateDescription?: string;
	CreationTime?: string | number;
	LastUpdatedTime?: string | number;
	DeletionTime?: string | number;
	ParentId?: string;
	RootId?: string;
	DriftInformation?: DriftInformation;
};
export type StackParameter = {
	ParameterKey: string;
	ParameterValue?: string;
	ResolvedValue?: string;
};
export type StackOutput = {
	OutputKey: string;
	OutputValue?: string;
	Description?: string;
	ExportName?: string;
};
export type Tag = { Key: string; Value: string };
export type Stack = {
	StackId: string;
	StackName: string;
	StackStatus: string;
	StackStatusReason?: string;
	Description?: string;
	CreationTime?: string | number;
	LastUpdatedTime?: string | number;
	DeletionTime?: string | number;
	ParentId?: string;
	RootId?: string;
	RoleARN?: string;
	DisableRollback?: boolean;
	EnableTerminationProtection?: boolean;
	Capabilities?: string[];
	Parameters?: StackParameter[];
	Outputs?: StackOutput[];
	Tags?: Tag[];
	DriftInformation?: DriftInformation;
};
export type StackEvent = {
	EventId: string;
	Timestamp?: string | number;
	LogicalResourceId?: string;
	PhysicalResourceId?: string;
	ResourceType?: string;
	ResourceStatus?: string;
	ResourceStatusReason?: string;
};
export type StackResource = {
	LogicalResourceId: string;
	PhysicalResourceId?: string;
	ResourceType: string;
	ResourceStatus: string;
	ResourceStatusReason?: string;
	LastUpdatedTimestamp?: string | number;
};
export type ChangeSetSummary = {
	ChangeSetId: string;
	ChangeSetName: string;
	Status?: string;
	ExecutionStatus?: string;
	StatusReason?: string;
	Description?: string;
	CreationTime?: string | number;
};
export type ResourceChange = {
	Action?: string;
	LogicalResourceId?: string;
	PhysicalResourceId?: string;
	ResourceType?: string;
	Replacement?: string;
	Scope?: string[];
};
export type ChangeSetDetail = ChangeSetSummary & {
	Parameters?: StackParameter[];
	Changes?: { Type?: string; ResourceChange?: ResourceChange }[];
};

export const deleteComplete = "DELETE_COMPLETE";
export const deleteFailed = "DELETE_FAILED";
export const nestedStackType = "AWS::CloudFormation::Stack";

// inProgress is true while a stack operation runs. REVIEW_IN_PROGRESS only
// waits for a change set to be executed, so there is nothing to follow.
export const inProgress = (status: string) =>
	status.endsWith("_IN_PROGRESS") && status !== "REVIEW_IN_PROGRESS";

export const failed = (status = "") => status.includes("FAILED");

export type StatusType =
	| "success"
	| "error"
	| "warning"
	| "stopped"
	| "pending"
	| "in-progress";

export function statusType(status: string): StatusType {
	if (failed(status)) return "error";
	if (status === "REVIEW_IN_PROGRESS") return "pending";
	if (inProgress(status)) return "in-progress";
	if (status === deleteComplete || status === "DELETE_SKIPPED")
		return "stopped";
	if (status.includes("ROLLBACK")) return "warning";
	if (status.endsWith("_COMPLETE")) return "success";
	return "pending";
}

// filterStacks hides deleted stacks unless includeDeleted is set or the
// status filter names DELETE_COMPLETE. An empty status shows every status.
export function filterStacks<T extends { StackStatus: string }>(
	stacks: T[],
	status: string,
	includeDeleted: boolean,
): T[] {
	return stacks.filter((s) =>
		status
			? s.StackStatus === status
			: includeDeleted || s.StackStatus !== deleteComplete,
	);
}

// stackNameOf reads the name out of a stack ID (arn:...:stack/name/uuid).
export const stackNameOf = (id: string) =>
	id.startsWith("arn:") ? (id.split("/")[1] ?? id) : id;

export function cfnDate(value?: string | number) {
	if (value === undefined || value === "") return "—";
	const date = new Date(typeof value === "number" ? value * 1000 : value);
	return Number.isNaN(date.getTime()) ? "—" : date.toISOString();
}

// TinyStack, like AWS, answers **** for the value of a NoEcho parameter.
export const noEchoValue = "****";

// findInventory picks the inventory record that a physical ID names in the
// stack's account and Region. The AWS service in the CloudFormation type
// breaks ties between records that share a name.
export function findInventory(
	inventory: Resource[],
	resource: Pick<StackResource, "PhysicalResourceId" | "ResourceType">,
	account: string,
	region: string,
): Resource | undefined {
	const id = resource.PhysicalResourceId;
	if (!id) return undefined;
	const service = resource.ResourceType.split("::")[1]?.toLowerCase();
	const found = inventory.filter(
		(r) =>
			r.account === account &&
			(r.region === region || r.region === "") &&
			(r.arn === id || r.name === id),
	);
	return found.find((r) => r.service === service) ?? found[0];
}
