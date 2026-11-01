import {
	Alert,
	Button,
	FormField,
	Input,
	KeyValuePairs,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { stringMap } from "#/lib/consoleActions";
import {
	type CacheCluster,
	cliCommand,
	defaultPort,
	type ElastiCacheScope,
	ecCall,
	ecDate,
	ecPages,
	endpointText,
	engineNames,
	familiesFor,
	nodeTypes,
	versionsFor,
} from "#/lib/elasticache";
import {
	clusterCreateInput,
	clusterModifyInput,
	hasPending,
} from "#/lib/elasticacheHelpers";
import { usePoll } from "#/lib/usePoll";
import {
	CacheList,
	type CacheOptions,
	CacheStatus,
	CacheTags,
	copyText,
	DetailBox,
	EndpointValue,
	MultiField,
	SelectField,
} from "./CacheKit";
import { TagsField } from "./TagsField";

export const clusterEndpoint = (c: CacheCluster) =>
	endpointText(c.ConfigurationEndpoint) ||
	endpointText(c.CacheNodes?.[0]?.Endpoint);

export function CacheClusters({
	scope,
	enabled,
	options,
	onChanged,
}: {
	scope: ElastiCacheScope;
	enabled: boolean;
	options: CacheOptions;
	onChanged: () => void;
}) {
	const [pending, setPending] = useState(false),
		[dialog, setDialog] = useState(""),
		[selectedID, setSelectedID] = useState("");
	const poll = usePoll(
		async () => {
			const items = enabled
				? await ecPages<CacheCluster>(
						scope,
						"DescribeCacheClusters",
						"CacheClusters",
						{ ShowCacheNodeInfo: true },
					)
				: [];
			setPending(hasPending(items.map((c) => c.CacheClusterStatus)));
			return items;
		},
		pending ? 3000 : 0,
	);
	const items = poll.data ?? [],
		selected = items.find((c) => c.CacheClusterId === selectedID);
	const changed = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	// Members of a replication group are managed from the Replication groups section.
	const standalone = selected && !selected.ReplicationGroupId;
	return (
		<>
			<CacheList
				title="Cache clusters"
				items={items}
				trackBy="CacheClusterId"
				loading={poll.loading}
				error={poll.error}
				emptyText="Choose Create cache cluster to add one."
				onRefresh={poll.reload}
				selected={selected}
				onSelect={(c) => setSelectedID(c?.CacheClusterId ?? "")}
				actions={
					<>
						<Button
							disabled={!enabled || !standalone}
							onClick={() => setDialog("modify")}
						>
							Modify
						</Button>
						<Button
							disabled={!enabled || !standalone}
							onClick={() => setDialog("reboot")}
						>
							Reboot
						</Button>
						<Button
							disabled={!enabled || !standalone}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog("create")}
						>
							Create cache cluster
						</Button>
					</>
				}
				columns={[
					{
						id: "id",
						header: "Cluster ID",
						sortingField: "CacheClusterId",
						isRowHeader: true,
						cell: (c) => c.CacheClusterId,
					},
					{
						id: "engine",
						header: "Engine and version",
						cell: (c) => `${c.Engine} ${c.EngineVersion}`,
					},
					{
						id: "type",
						header: "Node type",
						cell: (c) => c.CacheNodeType,
					},
					{
						id: "nodes",
						header: "Nodes",
						cell: (c) => c.NumCacheNodes,
					},
					{
						id: "status",
						header: "Status",
						cell: (c) => <CacheStatus status={c.CacheClusterStatus} />,
					},
					{
						id: "endpoint",
						header: "Endpoint",
						cell: (c) => clusterEndpoint(c) || "—",
					},
					{
						id: "created",
						header: "Created",
						cell: (c) => ecDate(c.CacheClusterCreateTime),
					},
				]}
				detail={
					selected && (
						<SpaceBetween size="l">
							<ClusterDetails
								cluster={selected}
								scope={scope}
								enabled={enabled}
							/>
						</SpaceBetween>
					)
				}
			/>
			{dialog === "create" && (
				<CreateCluster
					scope={scope}
					options={options}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "modify" && selected && (
				<ModifyCluster
					scope={scope}
					cluster={selected}
					options={options}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "reboot" && selected && (
				<ActionDialog
					title={`Reboot ${selected.CacheClusterId}`}
					submit="Reboot cluster"
					confirmation={selected.CacheClusterId}
					onClose={() => setDialog("")}
					onSaved={changed}
					plan={async () => [
						{
							label: "Reboot cluster",
							run: () =>
								ecCall(scope, "RebootCacheCluster", {
									CacheClusterId: selected.CacheClusterId,
									CacheNodeIdsToReboot: (selected.CacheNodes ?? []).map(
										(n) => n.CacheNodeId,
									),
								}),
						},
					]}
				>
					<Alert type="warning">
						Rebooting restarts every node. Cached data may be lost.
					</Alert>
				</ActionDialog>
			)}
			{dialog === "delete" && selected && (
				<ActionDialog
					title={`Delete ${selected.CacheClusterId}`}
					submit="Delete cluster"
					confirmation={selected.CacheClusterId}
					onClose={() => setDialog("")}
					onSaved={() => {
						setSelectedID("");
						changed();
					}}
					plan={async () => [
						{
							label: "Delete cluster",
							run: () =>
								ecCall(scope, "DeleteCacheCluster", {
									CacheClusterId: selected.CacheClusterId,
								}),
						},
					]}
				>
					<Alert type="warning">
						Deleting a cluster removes its nodes and data. Snapshots are not
						supported, so no final snapshot is taken.
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}

function ClusterDetails({
	cluster: c,
	scope,
	enabled,
}: {
	cluster: CacheCluster;
	scope: ElastiCacheScope;
	enabled: boolean;
}) {
	const endpoint = clusterEndpoint(c),
		command = cliCommand(c.Engine, endpoint);
	return (
		<SpaceBetween size="l">
			<DetailBox title={c.CacheClusterId}>
				<KeyValuePairs
					columns={3}
					items={[
						{
							label: "Status",
							value: <CacheStatus status={c.CacheClusterStatus} />,
						},
						{ label: "Engine", value: `${c.Engine} ${c.EngineVersion}` },
						{ label: "Node type", value: c.CacheNodeType },
						{ label: "Subnet group", value: c.CacheSubnetGroupName || "—" },
						{
							label: "Parameter group",
							value: c.CacheParameterGroup?.CacheParameterGroupName || "—",
						},
						{
							label: "Replication group",
							value: c.ReplicationGroupId || "—",
						},
						{ label: "Endpoint", value: <EndpointValue value={endpoint} /> },
						{
							label: "ARN",
							value: (
								<SpaceBetween direction="horizontal" size="xs">
									{c.ARN}
									{copyText(c.ARN)}
								</SpaceBetween>
							),
						},
						{
							label: "Connect",
							value: command ? (
								<SpaceBetween direction="horizontal" size="xs">
									<code>{command}</code>
									{copyText(command)}
								</SpaceBetween>
							) : (
								"—"
							),
						},
					]}
				/>
			</DetailBox>
			<Table
				variant="container"
				header={<>Nodes</>}
				items={c.CacheNodes ?? []}
				trackBy="CacheNodeId"
				empty="No nodes reported."
				columnDefinitions={[
					{ id: "id", header: "Node", cell: (n) => n.CacheNodeId },
					{
						id: "status",
						header: "Status",
						cell: (n) => <CacheStatus status={n.CacheNodeStatus} />,
					},
					{
						id: "endpoint",
						header: "Endpoint",
						cell: (n) => <EndpointValue value={endpointText(n.Endpoint)} />,
					},
				]}
			/>
			<CacheTags scope={scope} arn={c.ARN} enabled={enabled} />
		</SpaceBetween>
	);
}

function CreateCluster({
	scope,
	options,
	onClose,
	onSaved,
}: {
	scope: ElastiCacheScope;
	options: CacheOptions;
	onClose: () => void;
	onSaved: () => void;
}) {
	const engines = engineNames(options.versions);
	const [id, setId] = useState(""),
		[engine, setEngine] = useState(
			engines.includes("redis") ? "redis" : (engines[0] ?? ""),
		),
		[version, setVersion] = useState(""),
		[nodeType, setNodeType] = useState(nodeTypes[0]),
		[nodes, setNodes] = useState("1"),
		[port, setPort] = useState(String(defaultPort(engine))),
		[subnet, setSubnet] = useState(""),
		[parameter, setParameter] = useState(""),
		[groups, setGroups] = useState<string[]>([]),
		[tags, setTags] = useState("{}");
	const versions = versionsFor(options.versions, engine),
		families = new Set(
			familiesFor(options.versions.filter((v) => v.Engine === engine)),
		);
	return (
		<ActionDialog
			title="Create cache cluster"
			submit="Create cache cluster"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = clusterCreateInput({
					id,
					engine,
					version,
					nodeType,
					nodes,
					port,
					subnetGroup: subnet,
					parameterGroup: parameter,
					securityGroups: groups,
					tags: stringMap(tags, "Tags"),
				});
				return [
					{
						label: "Create cache cluster",
						run: () => ecCall(scope, "CreateCacheCluster", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<FormField
					label="Cluster ID"
					description="Lowercase letters, numbers and hyphens, starting with a letter."
				>
					<Input
						ariaLabel="Cluster ID"
						value={id}
						onChange={({ detail }) => setId(detail.value)}
					/>
				</FormField>
				<SelectField
					label="Engine"
					value={engine}
					options={engines}
					onChange={(v) => {
						setEngine(v);
						setVersion("");
						setParameter("");
						setPort(String(defaultPort(v)));
					}}
				/>
				<SelectField
					label="Engine version"
					value={version}
					options={versions}
					onChange={setVersion}
					empty="Choose an engine first"
				/>
				<SelectField
					label="Node type"
					value={nodeType}
					options={nodeTypes}
					onChange={setNodeType}
				/>
				{engine === "memcached" && (
					<FormField label="Number of nodes" description="1 to 40.">
						<Input
							ariaLabel="Number of nodes"
							inputMode="numeric"
							value={nodes}
							onChange={({ detail }) => setNodes(detail.value)}
						/>
					</FormField>
				)}
				<FormField label="Port">
					<Input
						ariaLabel="Port"
						inputMode="numeric"
						value={port}
						onChange={({ detail }) => setPort(detail.value)}
					/>
				</FormField>
				<SelectField
					label="Subnet group"
					description="Leave empty to use the default subnet group."
					value={subnet}
					options={options.subnetGroups.map((g) => g.CacheSubnetGroupName)}
					onChange={setSubnet}
					empty="No subnet groups"
				/>
				<SelectField
					label="Parameter group"
					description="Leave empty to use the engine default."
					value={parameter}
					options={options.parameterGroups
						.filter(
							(g) =>
								!g.CacheParameterGroupFamily ||
								families.has(g.CacheParameterGroupFamily),
						)
						.map((g) => g.CacheParameterGroupName)}
					onChange={setParameter}
					empty="No parameter groups for this engine"
				/>
				<MultiField
					label="Security groups"
					values={groups}
					options={options.securityGroups}
					onChange={setGroups}
					empty="No EC2 security groups"
				/>
				<TagsField value={tags} onChange={setTags} />
			</SpaceBetween>
		</ActionDialog>
	);
}

function ModifyCluster({
	scope,
	cluster,
	options,
	onClose,
	onSaved,
}: {
	scope: ElastiCacheScope;
	cluster: CacheCluster;
	options: CacheOptions;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [version, setVersion] = useState(cluster.EngineVersion),
		[nodes, setNodes] = useState(String(cluster.NumCacheNodes)),
		[parameter, setParameter] = useState("");
	const families = new Set(
		familiesFor(options.versions.filter((v) => v.Engine === cluster.Engine)),
	);
	return (
		<ActionDialog
			title={`Modify ${cluster.CacheClusterId}`}
			submit="Save changes"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = clusterModifyInput(cluster, {
					version,
					nodes,
					parameterGroup: parameter,
				});
				return [
					{
						label: "Modify cache cluster",
						run: () =>
							ecCall(scope, "ModifyCacheCluster", {
								CacheClusterId: cluster.CacheClusterId,
								...input,
							}),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					Node type cannot be changed after creation. Engine versions can only
					be upgraded.
				</Alert>
				<SelectField
					label="Engine version"
					value={version}
					options={versionsFor(options.versions, cluster.Engine)}
					onChange={setVersion}
				/>
				{cluster.Engine === "memcached" && (
					<FormField label="Number of nodes" description="1 to 40.">
						<Input
							ariaLabel="Number of nodes"
							inputMode="numeric"
							value={nodes}
							onChange={({ detail }) => setNodes(detail.value)}
						/>
					</FormField>
				)}
				<SelectField
					label="Parameter group"
					description="Leave empty to keep the current group."
					value={parameter}
					options={options.parameterGroups
						.filter(
							(g) =>
								!g.CacheParameterGroupFamily ||
								families.has(g.CacheParameterGroupFamily),
						)
						.map((g) => g.CacheParameterGroupName)}
					onChange={setParameter}
					empty="No parameter groups for this engine"
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
