import {
	Alert,
	Box,
	ColumnLayout,
	Container,
	CopyToClipboard,
	Header,
	SpaceBetween,
	StatusIndicator,
	Toggle,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, Text } from "#/components/memorydb/Fields";
import {
	type ACL,
	type Cluster,
	listClusters,
	type MemoryDBScope,
	memorydbCall,
	type SubnetGroup,
} from "#/lib/memorydb";
import {
	type ClusterForm,
	createClusterInput,
	defaultPort,
	describeShape,
	endpointText,
	engineOptions,
	findEngine,
	hasPendingClusters,
	isPendingStatus,
	nodeTypes,
	openAccessACL,
	replicasPerShard,
	subnetGroupNone,
	updateClusterInput,
	upgradeTargets,
} from "#/lib/memorydbHelpers";
import { usePoll } from "#/lib/usePoll";

const emptyForm: ClusterForm = {
	name: "",
	description: "",
	nodeType: "db.t4g.small",
	engine: "valkey 7.2",
	shards: "1",
	replicas: "1",
	acl: openAccessACL,
	subnetGroup: subnetGroupNone,
	tls: true,
	port: String(defaultPort),
};

function statusType(status?: string) {
	if (status === "available") return "success";
	if (isPendingStatus(status)) return "in-progress";
	return status?.includes("failed") ? "error" : "info";
}

export function MemoryDBClusters({
	scope,
	enabled,
	acls,
	subnetGroups,
	onChanged,
}: {
	scope: MemoryDBScope;
	enabled: boolean;
	acls: ACL[];
	subnetGroups: SubnetGroup[];
	onChanged: () => void;
}) {
	const [pending, setPending] = useState(false);
	const poll = usePoll(
		async () => {
			const items = enabled ? await listClusters(scope) : [];
			setPending(hasPendingClusters(items));
			return items;
		},
		pending ? 3000 : 0,
	);
	const [selected, setSelected] = useState(""),
		[dialog, setDialog] = useState<"" | "create" | "update" | "delete">(""),
		[target, setTarget] = useState<Cluster>(),
		[form, setForm] = useState<ClusterForm>(emptyForm);
	const clusters = poll.data ?? [];
	const detail = clusters.find((c) => c.Name === selected);
	const set = (patch: Partial<ClusterForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	const changed = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	const open = (kind: "create" | "update" | "delete", c?: Cluster) => {
		setTarget(c);
		if (kind === "create") setForm(emptyForm);
		if (kind === "update" && c)
			setForm({
				name: c.Name,
				description: c.Description ?? "",
				nodeType: c.NodeType ?? "",
				engine: `${c.Engine} ${c.EngineVersion}`,
				shards: String(c.NumberOfShards ?? 1),
				replicas: String(replicasPerShard(c)),
				acl: c.ACLName ?? "",
				subnetGroup: c.SubnetGroupName ?? subnetGroupNone,
				tls: c.TLSEnabled ?? true,
				port: String(c.ClusterEndpoint?.Port ?? defaultPort),
			});
		setDialog(kind);
	};
	const engine = findEngine(form.engine);
	const engines =
		dialog === "update" && target
			? upgradeTargets(target.Engine ?? "", target.EngineVersion ?? "")
			: engineOptions;
	return (
		<SpaceBetween size="l">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Clusters"
				emptyText="Choose Create cluster to get started."
				items={clusters}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Create cluster
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (c) => (
							<Action onClick={() => setSelected(c.Name)}>{c.Name}</Action>
						),
					},
					{
						id: "status",
						header: "Status",
						cell: (c) => (
							<StatusIndicator type={statusType(c.Status)}>
								{c.Status}
							</StatusIndicator>
						),
					},
					{
						id: "engine",
						header: "Engine",
						cell: (c) => `${c.Engine} ${c.EngineVersion}`,
					},
					{ id: "node", header: "Node type", cell: (c) => c.NodeType },
					{ id: "shape", header: "Shards and replicas", cell: describeShape },
					{
						id: "endpoint",
						header: "Cluster endpoint",
						cell: (c) =>
							endpointText(c) ? (
								<CopyToClipboard
									variant="inline"
									textToCopy={endpointText(c)}
									copySuccessText="Endpoint copied"
									copyErrorText="Endpoint failed to copy"
								/>
							) : (
								"-"
							),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (c) => (
							<Buttons>
								<Action
									disabled={!enabled || isPendingStatus(c.Status)}
									onClick={() => open("update", c)}
								>
									Update
								</Action>
								<Action disabled={!enabled} onClick={() => open("delete", c)}>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{detail && (
				<Container
					header={
						<Header variant="h2" description={detail.ARN}>
							{detail.Name}
						</Header>
					}
				>
					<ColumnLayout columns={3} variant="text-grid">
						{[
							["Status", detail.Status],
							["Engine", `${detail.Engine} ${detail.EngineVersion}`],
							["Node type", detail.NodeType],
							["Availability", detail.AvailabilityMode],
							["Shards and replicas", describeShape(detail)],
							["TLS", detail.TLSEnabled ? "Enabled" : "Disabled"],
							["Access control list", detail.ACLName],
							["Parameter group", detail.ParameterGroupName],
							["Subnet group", detail.SubnetGroupName || "None"],
							["Description", detail.Description || "-"],
						].map(([label, value]) => (
							<div key={label}>
								<Box variant="awsui-key-label">{label}</Box>
								<div>{value}</div>
							</div>
						))}
						<div>
							<Box variant="awsui-key-label">Cluster endpoint</Box>
							{endpointText(detail) ? (
								<CopyToClipboard
									variant="inline"
									textToCopy={endpointText(detail)}
									copySuccessText="Endpoint copied"
									copyErrorText="Endpoint failed to copy"
								/>
							) : (
								"-"
							)}
						</div>
					</ColumnLayout>
				</Container>
			)}
			{(dialog === "create" || dialog === "update") && (
				<ActionDialog
					title={dialog === "create" ? "Create cluster" : `Update ${form.name}`}
					submit={dialog === "create" ? "Create cluster" : "Update cluster"}
					onClose={() => setDialog("")}
					onSaved={() => {
						if (dialog === "create")
							setSelected(form.name.trim().toLowerCase());
						changed();
					}}
					plan={async () => {
						const input =
							dialog === "create" || !target
								? createClusterInput(form)
								: updateClusterInput(target, form, replicasPerShard(target));
						const operation =
							dialog === "create" ? "CreateCluster" : "UpdateCluster";
						return [
							{
								label: operation,
								run: () => memorydbCall(scope, operation, input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						{dialog === "update" && (
							<Alert type="info">
								Port, TLS and the subnet group cannot change after creation.
								Engines can only move to a later version.
							</Alert>
						)}
						<Text
							label="Cluster name"
							value={form.name}
							disabled={dialog === "update"}
							onChange={(name) => set({ name })}
							constraint="1 to 40 letters, digits or hyphens; begins with a letter."
						/>
						<Text
							label="Description"
							value={form.description}
							onChange={(description) => set({ description })}
						/>
						<Pick
							label="Node type"
							value={form.nodeType}
							placeholder="Choose a node type"
							options={nodeTypes.map((value) => ({ value }))}
							onChange={(nodeType) => set({ nodeType })}
						/>
						<Pick
							label="Engine version"
							value={form.engine}
							placeholder="Choose an engine version"
							options={engines.map((e) => ({ value: e.label }))}
							onChange={(engine) => set({ engine })}
						/>
						<Pick
							label="Parameter group"
							value={engine?.parameterGroup ?? ""}
							placeholder="Choose an engine version first"
							description="Only the default group of the engine version is available."
							options={engine ? [{ value: engine.parameterGroup }] : []}
							onChange={() => undefined}
						/>
						<Text
							label="Number of shards"
							value={form.shards}
							onChange={(shards) => set({ shards })}
							constraint="1 to 500."
						/>
						<Text
							label="Replicas per shard"
							value={form.replicas}
							onChange={(replicas) => set({ replicas })}
							constraint="0 to 5."
						/>
						<Pick
							label="Access control list"
							value={form.acl}
							placeholder="Choose an ACL"
							options={acls.map((a) => ({ value: a.Name }))}
							onChange={(acl) => set({ acl })}
						/>
						{dialog === "create" && (
							<>
								<Pick
									label="Subnet group"
									value={form.subnetGroup}
									placeholder="Choose a subnet group"
									options={[
										{ value: subnetGroupNone },
										...subnetGroups.map((g) => ({ value: g.Name })),
									]}
									onChange={(subnetGroup) => set({ subnetGroup })}
								/>
								<Toggle
									checked={form.tls}
									onChange={({ detail }) => set({ tls: detail.checked })}
									description={`Without TLS the cluster must use the ${openAccessACL} ACL.`}
								>
									TLS enabled
								</Toggle>
								<Text
									label="Port"
									value={form.port}
									onChange={(port) => set({ port })}
								/>
							</>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && target && (
				<ActionDialog
					title="Delete cluster"
					submit="Delete cluster"
					confirmation={target.Name}
					onClose={() => setDialog("")}
					onSaved={() => {
						if (selected === target.Name) setSelected("");
						changed();
					}}
					plan={async () => [
						{
							label: "DeleteCluster",
							run: () =>
								memorydbCall(scope, "DeleteCluster", {
									ClusterName: target.Name,
								}),
						},
					]}
				>
					<Alert type="warning">
						Deleting {target.Name} removes the cluster and its data. TinyStack
						does not support final snapshots, so none is taken.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
