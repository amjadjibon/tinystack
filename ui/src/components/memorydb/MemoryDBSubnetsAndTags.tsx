import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, PickMany, Text } from "#/components/memorydb/Fields";
import {
	type ACL,
	listClusters,
	listSubnets,
	type MemoryDBScope,
	memorydbCall,
	type SubnetGroup,
	type User,
} from "#/lib/memorydb";
import { subnetGroupInput, subnetLabel } from "#/lib/memorydbHelpers";
import { tagList, tagMap } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";

type Dialog = "" | "create" | "update" | "delete";

export function MemoryDBSubnetGroups({
	scope,
	enabled,
	groups,
	loading,
	onChanged,
}: {
	scope: MemoryDBScope;
	enabled: boolean;
	groups: SubnetGroup[];
	loading: boolean;
	onChanged: () => void;
}) {
	const subnets = usePoll(
		() => (enabled ? listSubnets(scope) : Promise.resolve([])),
		0,
	);
	const [dialog, setDialog] = useState<Dialog>(""),
		[target, setTarget] = useState<SubnetGroup>(),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[ids, setIds] = useState<string[]>([]);
	const open = (kind: Dialog, group?: SubnetGroup) => {
		setTarget(group);
		setName(group?.Name ?? "");
		setDescription(group?.Description ?? "");
		setIds((group?.Subnets ?? []).map((s) => s.Identifier ?? ""));
		setDialog(kind);
	};
	const known = subnets.data ?? [];
	return (
		<>
			<Rows
				title="Subnet groups"
				emptyText="Create EC2 subnets first, then choose Create subnet group."
				items={groups}
				loading={loading}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Create subnet group
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (g) => g.Name },
					{ id: "desc", header: "Description", cell: (g) => g.Description },
					{ id: "vpc", header: "VPC", cell: (g) => g.VpcId },
					{
						id: "subnets",
						header: "Subnets",
						cell: (g) =>
							(g.Subnets ?? []).map((s) => s.Identifier).join(", ") || "-",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (g) => (
							<Buttons>
								<Action disabled={!enabled} onClick={() => open("update", g)}>
									Update
								</Action>
								<Action disabled={!enabled} onClick={() => open("delete", g)}>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{(dialog === "create" || dialog === "update") && (
				<ActionDialog
					title={dialog === "create" ? "Create subnet group" : `Update ${name}`}
					submit={
						dialog === "create" ? "Create subnet group" : "Update subnet group"
					}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => {
						const operation =
							dialog === "create" ? "CreateSubnetGroup" : "UpdateSubnetGroup";
						const input =
							dialog === "create"
								? subnetGroupInput(name, description, ids, known)
								: {
										SubnetGroupName: target?.Name,
										...subnetGroupInput(undefined, description, ids, known),
									};
						return [
							{
								label: operation,
								run: () => memorydbCall(scope, operation, input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						{!!subnets.error && <Alert type="error">{subnets.error}</Alert>}
						<Text
							label="Subnet group name"
							value={name}
							disabled={dialog === "update"}
							onChange={setName}
							constraint="Begins with a letter; letters, digits and hyphens."
						/>
						<Text
							label="Description"
							value={description}
							onChange={setDescription}
						/>
						<PickMany
							label="Subnets"
							values={ids}
							placeholder="Choose subnets"
							description="All subnets must belong to the same VPC."
							options={[
								...known.map((s) => ({
									value: s.SubnetId ?? "",
									label: subnetLabel(s),
								})),
								...ids
									.filter((id) => !known.some((s) => s.SubnetId === id))
									.map((id) => ({ value: id })),
							]}
							onChange={setIds}
						/>
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && target && (
				<ActionDialog
					title="Delete subnet group"
					submit="Delete subnet group"
					confirmation={target.Name}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteSubnetGroup",
							run: () =>
								memorydbCall(scope, "DeleteSubnetGroup", {
									SubnetGroupName: target.Name,
								}),
						},
					]}
				>
					<Alert type="warning">
						A subnet group that a cluster uses cannot be deleted.
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}

type Kind = "Cluster" | "User" | "ACL" | "Subnet group";

export function MemoryDBTags({
	scope,
	enabled,
	users,
	acls,
	groups,
}: {
	scope: MemoryDBScope;
	enabled: boolean;
	users: User[];
	acls: ACL[];
	groups: SubnetGroup[];
}) {
	const [kind, setKind] = useState<Kind>("Cluster"),
		[arn, setArn] = useState("");
	const clusters = usePoll(
		() => (enabled ? listClusters(scope) : Promise.resolve([])),
		0,
	);
	const options: Record<Kind, { value: string; label: string }[]> = {
		Cluster: (clusters.data ?? []).map((c) => ({
			value: c.ARN,
			label: c.Name,
		})),
		// The built-in default user and open-access ACL cannot be tagged.
		User: users
			.filter((u) => u.Name !== "default")
			.map((u) => ({ value: u.ARN, label: u.Name })),
		ACL: acls
			.filter((a) => a.Name !== "open-access")
			.map((a) => ({ value: a.ARN, label: a.Name })),
		"Subnet group": groups.map((g) => ({ value: g.ARN, label: g.Name })),
	};
	return (
		<SpaceBetween size="m">
			<Pick
				label="Resource type"
				value={kind}
				placeholder="Choose a type"
				options={(["Cluster", "User", "ACL", "Subnet group"] as Kind[]).map(
					(value) => ({ value }),
				)}
				onChange={(v) => {
					setKind(v as Kind);
					setArn("");
				}}
			/>
			<Pick
				label="Resource"
				value={arn}
				placeholder="Choose a resource"
				options={options[kind]}
				onChange={setArn}
			/>
			{!!arn && <TagView key={arn} scope={scope} arn={arn} enabled={enabled} />}
		</SpaceBetween>
	);
}

function TagView({
	scope,
	arn,
	enabled,
}: {
	scope: MemoryDBScope;
	arn: string;
	enabled: boolean;
}) {
	const poll = usePoll(async () => {
		if (!enabled) return {};
		const out = await memorydbCall<{
			TagList?: { Key?: string; Value?: string }[];
		}>(scope, "ListTags", { ResourceArn: arn });
		return tagMap(out.TagList);
	}, 0);
	const { error, reload } = poll,
		tags = poll.data ?? {};
	return (
		<SpaceBetween size="m">
			{!!error && <Alert type="error">{error}</Alert>}
			<ConsoleTags
				tags={tags}
				enabled={enabled}
				onChanged={reload}
				plan={(next, removed) => [
					...(Object.keys(next).length
						? [
								{
									label: "TagResource",
									run: () =>
										memorydbCall(scope, "TagResource", {
											ResourceArn: arn,
											Tags: tagList(next),
										}),
								},
							]
						: []),
					...(removed.length
						? [
								{
									label: "UntagResource",
									run: () =>
										memorydbCall(scope, "UntagResource", {
											ResourceArn: arn,
											TagKeys: removed,
										}),
								},
							]
						: []),
				]}
			/>
		</SpaceBetween>
	);
}
