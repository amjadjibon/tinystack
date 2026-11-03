import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	Action,
	Buttons,
	Choice,
	Field,
	Rows,
} from "#/components/containers/Controls";
import { PickMany } from "#/components/memorydb/Fields";
import { stringMap, words } from "#/lib/consoleActions";
import { eksCall, eksList } from "#/lib/eks";
import {
	type Cluster,
	type EKSScope,
	pending,
	resourceName,
	role,
	scaling,
} from "#/lib/eksHelpers";
import { unixTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";

type Item = {
	[key: string]: unknown;
	Status?: string;
	CreatedAt?: number;
	ScalingConfig?: { MinSize?: number; MaxSize?: number; DesiredSize?: number };
};
const kinds = {
	nodegroups: {
		title: "Node groups",
		singular: "Nodegroup",
		list: "Nodegroups",
		name: "NodegroupName",
	},
	addons: {
		title: "Add-ons",
		singular: "Addon",
		list: "Addons",
		name: "AddonName",
	},
	fargate: {
		title: "Fargate profiles",
		singular: "FargateProfile",
		list: "FargateProfileNames",
		name: "FargateProfileName",
	},
	access: {
		title: "Access entries",
		singular: "AccessEntry",
		list: "AccessEntries",
		name: "PrincipalArn",
	},
};
export type ResourceKind = keyof typeof kinds;
export function Resources({
	scope,
	cluster,
	kind,
	enabled,
	onChanged,
}: {
	scope: EKSScope;
	cluster: Cluster;
	kind: ResourceKind;
	enabled: boolean;
	onChanged: () => void;
}) {
	const spec = kinds[kind];
	const [changing, setChanging] = useState(false);
	const poll = usePoll(
		async () => {
			if (
				!enabled ||
				(kind === "access" &&
					cluster.AccessConfig?.AuthenticationMode === "CONFIG_MAP")
			)
				return [];
			const names = await eksList<string>(
				scope,
				`List${kind === "fargate" ? "FargateProfiles" : spec.list}`,
				spec.list,
				{ ClusterName: cluster.Name },
			);
			return Promise.all(
				names.map(
					async (name) =>
						(
							await eksCall<Record<string, Item>>(
								scope,
								`Describe${spec.singular}`,
								{ ClusterName: cluster.Name, [spec.name]: name },
							)
						)[spec.singular],
				),
			);
		},
		changing ? 2000 : 0,
	);
	// A successful mutation reloads immediately; transitional records continue polling.
	const isPending = (poll.data ?? []).some((i) => pending(i.Status));
	const [dialog, setDialog] = useState<{
		op: "create" | "delete" | "scale";
		item?: Item;
	}>();
	const [principal, setPrincipal] = useState("");
	const saved = () => {
		setDialog(undefined);
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			<Pending active={isPending} onChange={setChanging} />
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{kind === "access" &&
				cluster.AccessConfig?.AuthenticationMode === "CONFIG_MAP" && (
					<Alert>
						Enable API or API_AND_CONFIG_MAP authentication in cluster
						configuration to manage access entries.
					</Alert>
				)}
			<Rows
				title={spec.title}
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText={`Choose Create to add ${spec.title.toLowerCase()}.`}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={
								!enabled ||
								(kind === "access" &&
									cluster.AccessConfig?.AuthenticationMode === "CONFIG_MAP")
							}
							onClick={() => setDialog({ op: "create" })}
						>
							Create
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name / principal",
						cell: (i) => String(i[spec.name] ?? "-"),
					},
					{ id: "status", header: "Status", cell: (i) => i.Status ?? "-" },
					{
						id: "created",
						header: "Created",
						cell: (i) => unixTime(i.CreatedAt),
					},
					{
						id: "details",
						header: "Configuration",
						cell: (i) =>
							JSON.stringify(
								kind === "nodegroups"
									? i.ScalingConfig
									: kind === "addons"
										? i.AddonVersion
										: kind === "access"
											? { Type: i.Type, Groups: i.KubernetesGroups }
											: i.Selectors,
							),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (i) => (
							<Buttons>
								{kind === "nodegroups" && (
									<Action
										disabled={!enabled}
										onClick={() => setDialog({ op: "scale", item: i })}
									>
										Edit scaling
									</Action>
								)}
								{kind === "access" && (
									<Action onClick={() => setPrincipal(String(i.PrincipalArn))}>
										Policies
									</Action>
								)}
								<Action
									disabled={!enabled}
									onClick={() => setDialog({ op: "delete", item: i })}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{!!dialog && (
				<ResourceDialog
					key={`${dialog.op}/${String(dialog.item?.[spec.name])}`}
					scope={scope}
					cluster={cluster}
					kind={kind}
					{...dialog}
					onClose={() => setDialog(undefined)}
					onSaved={saved}
				/>
			)}
			{!!principal && (
				<Policies
					key={principal}
					scope={scope}
					cluster={cluster.Name}
					principal={principal}
					enabled={enabled}
					onClose={() => setPrincipal("")}
					onChanged={onChanged}
				/>
			)}
		</SpaceBetween>
	);
}

import { useEffect } from "react";

function Pending({
	active,
	onChange,
}: {
	active: boolean;
	onChange: (active: boolean) => void;
}) {
	useEffect(() => {
		onChange(active);
	}, [active, onChange]);
	return null;
}
function ResourceDialog({
	scope,
	cluster,
	kind,
	op,
	item,
	onClose,
	onSaved,
}: {
	scope: EKSScope;
	cluster: Cluster;
	kind: ResourceKind;
	op: "create" | "delete" | "scale";
	item?: Item;
	onClose: () => void;
	onSaved: () => void;
}) {
	const spec = kinds[kind];
	const [f, setF] = useState({
		name: "",
		role: "",
		subnets: [] as string[],
		min: String(item?.ScalingConfig?.MinSize ?? 1),
		max: String(item?.ScalingConfig?.MaxSize ?? 2),
		desired: String(item?.ScalingConfig?.DesiredSize ?? 1),
		instances: "t3.medium",
		capacity: "ON_DEMAND",
		ami: "AL2023_x86_64_STANDARD",
		labels: "{}",
		namespace: "default",
		groups: "",
		username: "",
		addon: "",
	});
	const set = (p: Partial<typeof f>) => setF((v) => ({ ...v, ...p }));
	const catalog = usePoll(
		() =>
			kind === "addons" && op === "create"
				? eksList<{ AddonName: string }>(
						scope,
						"DescribeAddonVersions",
						"Addons",
						{ KubernetesVersion: cluster.Version },
					)
				: Promise.resolve([]),
		0,
	);
	const name = String(item?.[spec.name] ?? "");
	const operation =
		op === "scale"
			? "UpdateNodegroupConfig"
			: `${op === "delete" ? "Delete" : "Create"}${spec.singular}`;
	return (
		<ActionDialog
			title={`${op} ${spec.title.toLowerCase()}`}
			confirmation={op === "delete" ? name : undefined}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				let input: Record<string, unknown> = {
					ClusterName: cluster.Name,
					[spec.name]: name,
				};
				if (op === "scale")
					input.ScalingConfig = scaling(f.min, f.max, f.desired);
				if (op === "create") {
					input = {
						ClusterName: cluster.Name,
						[spec.name]:
							kind === "access"
								? f.name.trim()
								: resourceName((kind === "addons" ? f.addon : f.name).trim()),
					};
					if (kind === "nodegroups") {
						if (!f.subnets.length)
							throw new Error("Choose at least one subnet.");
						Object.assign(input, {
							NodeRole: role(f.role),
							Subnets: f.subnets,
							ScalingConfig: scaling(f.min, f.max, f.desired),
							InstanceTypes: words(f.instances),
							CapacityType: f.capacity,
							AmiType: f.ami,
							Labels: stringMap(f.labels, "Labels"),
						});
					}
					if (kind === "fargate")
						Object.assign(input, {
							PodExecutionRoleArn: role(f.role),
							Subnets: f.subnets,
							Selectors: [
								{
									Namespace: f.namespace,
									Labels: stringMap(f.labels, "Labels"),
								},
							],
						});
					if (kind === "access")
						Object.assign(input, {
							Type: "STANDARD",
							KubernetesGroups: words(f.groups),
							...(f.username ? { Username: f.username } : {}),
						});
				}
				return [
					{ label: operation, run: () => eksCall(scope, operation, input) },
				];
			}}
		>
			<SpaceBetween size="m">
				{op === "delete" ? (
					<Alert type="warning">Delete {name} permanently.</Alert>
				) : (
					<SpaceBetween size="m">
						{!!catalog.error && <Alert type="error">{catalog.error}</Alert>}
						{op === "create" &&
							(kind === "addons" ? (
								<Choice
									label="Add-on"
									value={f.addon}
									options={(catalog.data ?? []).map((a) => a.AddonName)}
									onChange={(addon) => set({ addon })}
								/>
							) : (
								<Field
									label={kind === "access" ? "Principal ARN" : "Name"}
									value={f.name}
									onChange={(name) => set({ name })}
								/>
							))}
						{op === "create" &&
							(kind === "nodegroups" || kind === "fargate") && (
								<SpaceBetween size="m">
									<Field
										label={
											kind === "fargate"
												? "Pod execution role ARN"
												: "Node role ARN"
										}
										value={f.role}
										onChange={(role) => set({ role })}
									/>
									<PickMany
										label="Subnets"
										placeholder="Choose cluster subnets"
										values={f.subnets}
										options={(cluster.ResourcesVpcConfig?.SubnetIds ?? []).map(
											(value) => ({ value }),
										)}
										onChange={(subnets) => set({ subnets })}
									/>
									<Field
										label="Labels (JSON string map)"
										value={f.labels}
										onChange={(labels) => set({ labels })}
									/>
								</SpaceBetween>
							)}
						{kind === "nodegroups" && (
							<SpaceBetween size="m">
								<Field
									label="Minimum size"
									value={f.min}
									onChange={(min) => set({ min })}
								/>
								<Field
									label="Maximum size"
									value={f.max}
									onChange={(max) => set({ max })}
								/>
								<Field
									label="Desired size"
									value={f.desired}
									onChange={(desired) => set({ desired })}
								/>
								{op === "create" && (
									<SpaceBetween size="m">
										<Field
											label="Instance types (comma separated)"
											value={f.instances}
											onChange={(instances) => set({ instances })}
										/>
										<Choice
											label="Capacity type"
											value={f.capacity}
											options={["ON_DEMAND", "SPOT"]}
											onChange={(capacity) => set({ capacity })}
										/>
										<Field
											label="AMI type"
											value={f.ami}
											onChange={(ami) => set({ ami })}
										/>
									</SpaceBetween>
								)}
							</SpaceBetween>
						)}
						{kind === "fargate" && (
							<Field
								label="Namespace selector"
								value={f.namespace}
								onChange={(namespace) => set({ namespace })}
							/>
						)}
						{kind === "access" && (
							<SpaceBetween size="m">
								<Field
									label="Kubernetes groups (comma separated)"
									value={f.groups}
									onChange={(groups) => set({ groups })}
								/>
								<Field
									label="Username (optional)"
									value={f.username}
									onChange={(username) => set({ username })}
								/>
							</SpaceBetween>
						)}
					</SpaceBetween>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
function Policies({
	scope,
	cluster,
	principal,
	enabled,
	onClose,
	onChanged,
}: {
	scope: EKSScope;
	cluster: string;
	principal: string;
	enabled: boolean;
	onClose: () => void;
	onChanged: () => void;
}) {
	const input = { ClusterName: cluster, PrincipalArn: principal };
	const poll = usePoll(
		() =>
			eksList<{ PolicyArn: string; AccessScope?: unknown }>(
				scope,
				"ListAssociatedAccessPolicies",
				"AssociatedAccessPolicies",
				input,
			),
		0,
	);
	const available = usePoll(
		() =>
			eksList<{ Arn: string; Name: string }>(
				scope,
				"ListAccessPolicies",
				"AccessPolicies",
			),
		0,
	);
	const [policy, setPolicy] = useState("");
	const [namespaces, setNamespaces] = useState("");
	const [scopeType, setScopeType] = useState("cluster");
	const [action, setAction] = useState<{ arn: string; remove: boolean }>();
	return (
		<SpaceBetween size="m">
			<Action onClick={onClose}>Close policies for {principal}</Action>
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!!available.error && <Alert type="error">{available.error}</Alert>}
			<Rows
				title="Associated access policies"
				items={poll.data ?? []}
				emptyText="Associate a policy below."
				actions={<Action onClick={poll.reload}>Refresh</Action>}
				columns={[
					{ id: "arn", header: "Policy ARN", cell: (p) => p.PolicyArn },
					{
						id: "scope",
						header: "Scope",
						cell: (p) => JSON.stringify(p.AccessScope),
					},
					{
						id: "remove",
						header: "Actions",
						cell: (p) => (
							<Action
								disabled={!enabled}
								onClick={() => setAction({ arn: p.PolicyArn, remove: true })}
							>
								Disassociate
							</Action>
						),
					},
				]}
			/>
			<Choice
				label="Policy"
				value={policy}
				options={(available.data ?? []).map((p) => p.Arn)}
				onChange={setPolicy}
			/>
			<Choice
				label="Access scope"
				value={scopeType}
				options={["cluster", "namespace"]}
				onChange={setScopeType}
			/>
			{scopeType === "namespace" && (
				<Field
					label="Namespaces (comma separated)"
					value={namespaces}
					onChange={setNamespaces}
				/>
			)}
			<Action
				disabled={!enabled || !policy}
				onClick={() => setAction({ arn: policy, remove: false })}
			>
				Associate policy
			</Action>
			{!!action && (
				<ActionDialog
					title={action.remove ? "Disassociate policy" : "Associate policy"}
					confirmation={action.remove ? action.arn : undefined}
					onClose={() => setAction(undefined)}
					onSaved={() => {
						setAction(undefined);
						poll.reload();
						onChanged();
					}}
					plan={async () => {
						if (
							!action.remove &&
							scopeType === "namespace" &&
							!words(namespaces).length
						)
							throw new Error("Enter at least one namespace.");
						const op = action.remove
							? "DisassociateAccessPolicy"
							: "AssociateAccessPolicy";
						return [
							{
								label: op,
								run: () =>
									eksCall(scope, op, {
										...input,
										PolicyArn: action.arn,
										...(!action.remove
											? {
													AccessScope: {
														Type: scopeType,
														Namespaces:
															scopeType === "namespace"
																? words(namespaces)
																: [],
													},
												}
											: {}),
									}),
							},
						];
					}}
				>
					<Alert>{action.arn}</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
