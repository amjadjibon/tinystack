import {
	Alert,
	Button,
	Checkbox,
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
	type ReplicationGroup,
	versionsFor,
} from "#/lib/elasticache";
import {
	groupCreateInput,
	hasPending,
	replicaCountStep,
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

const replicaCount = (g: ReplicationGroup) =>
	Math.max((g.MemberClusters?.length ?? 1) - 1, 0);
const primaryEndpoint = (g: ReplicationGroup) =>
	endpointText(g.NodeGroups?.[0]?.PrimaryEndpoint);

export function ReplicationGroups({
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
				? await ecPages<ReplicationGroup>(
						scope,
						"DescribeReplicationGroups",
						"ReplicationGroups",
					)
				: [];
			setPending(hasPending(items.map((g) => g.Status)));
			return items;
		},
		pending ? 3000 : 0,
	);
	const items = poll.data ?? [],
		selected = items.find((g) => g.ReplicationGroupId === selectedID);
	const changed = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	const pick = (name: string) => (
		<Button
			disabled={!enabled || !selected}
			onClick={() => setDialog(name.toLowerCase())}
		>
			{name}
		</Button>
	);
	return (
		<>
			<CacheList
				title="Replication groups"
				items={items}
				trackBy="ReplicationGroupId"
				loading={poll.loading}
				error={poll.error}
				emptyText="Choose Create replication group to add one."
				onRefresh={poll.reload}
				selected={selected}
				onSelect={(g) => setSelectedID(g?.ReplicationGroupId ?? "")}
				actions={
					<>
						{pick("Modify")}
						{pick("Scale")}
						{pick("Failover")}
						{pick("Delete")}
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog("create")}
						>
							Create replication group
						</Button>
					</>
				}
				columns={[
					{
						id: "id",
						header: "Replication group",
						sortingField: "ReplicationGroupId",
						isRowHeader: true,
						cell: (g) => g.ReplicationGroupId,
					},
					{
						id: "description",
						header: "Description",
						cell: (g) => g.Description,
					},
					{
						id: "status",
						header: "Status",
						cell: (g) => <CacheStatus status={g.Status} />,
					},
					{ id: "engine", header: "Engine", cell: (g) => g.Engine ?? "—" },
					{
						id: "type",
						header: "Node type",
						cell: (g) => g.CacheNodeType ?? "—",
					},
					{ id: "replicas", header: "Replicas", cell: replicaCount },
					{
						id: "endpoint",
						header: "Primary endpoint",
						cell: (g) => primaryEndpoint(g) || "—",
					},
					{
						id: "created",
						header: "Created",
						cell: (g) => ecDate(g.ReplicationGroupCreateTime),
					},
				]}
				detail={
					selected && (
						<GroupDetails group={selected} scope={scope} enabled={enabled} />
					)
				}
			/>
			{dialog === "create" && (
				<CreateGroup
					scope={scope}
					options={options}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "modify" && selected && (
				<ModifyGroup
					scope={scope}
					group={selected}
					options={options}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "scale" && selected && (
				<ScaleGroup
					scope={scope}
					group={selected}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "failover" && selected && (
				<ActionDialog
					title={`Test failover for ${selected.ReplicationGroupId}`}
					submit="Start failover"
					confirmation={selected.ReplicationGroupId}
					onClose={() => setDialog("")}
					onSaved={changed}
					plan={async () => [
						{
							label: "Test failover",
							run: () =>
								ecCall(scope, "TestFailover", {
									ReplicationGroupId: selected.ReplicationGroupId,
									NodeGroupId: selected.NodeGroups?.[0]?.NodeGroupId ?? "0001",
								}),
						},
					]}
				>
					<Alert type="warning">
						Promotes a replica to primary. The group needs at least one replica.
					</Alert>
				</ActionDialog>
			)}
			{dialog === "delete" && selected && (
				<DeleteGroup
					scope={scope}
					group={selected}
					onClose={() => setDialog("")}
					onSaved={() => {
						setSelectedID("");
						changed();
					}}
				/>
			)}
		</>
	);
}

function GroupDetails({
	group: g,
	scope,
	enabled,
}: {
	group: ReplicationGroup;
	scope: ElastiCacheScope;
	enabled: boolean;
}) {
	const primary = primaryEndpoint(g),
		reader = endpointText(g.NodeGroups?.[0]?.ReaderEndpoint),
		engine = g.Engine ?? "redis",
		command = cliCommand(engine, primary);
	return (
		<SpaceBetween size="l">
			<DetailBox title={g.ReplicationGroupId}>
				<KeyValuePairs
					columns={3}
					items={[
						{ label: "Status", value: <CacheStatus status={g.Status} /> },
						{ label: "Automatic failover", value: g.AutomaticFailover ?? "—" },
						{ label: "Multi-AZ", value: g.MultiAZ ?? "—" },
						{
							label: "Primary endpoint",
							value: <EndpointValue value={primary} />,
						},
						{
							label: "Reader endpoint",
							value: <EndpointValue value={reader} />,
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
						{
							label: "ARN",
							value: (
								<SpaceBetween direction="horizontal" size="xs">
									{g.ARN}
									{copyText(g.ARN)}
								</SpaceBetween>
							),
						},
					]}
				/>
			</DetailBox>
			<Table
				variant="container"
				header={<>Members</>}
				items={g.NodeGroups?.[0]?.NodeGroupMembers ?? []}
				trackBy="CacheClusterId"
				empty="No members reported."
				columnDefinitions={[
					{ id: "id", header: "Cluster", cell: (m) => m.CacheClusterId },
					{ id: "role", header: "Role", cell: (m) => m.CurrentRole ?? "—" },
					{
						id: "endpoint",
						header: "Endpoint",
						cell: (m) => <EndpointValue value={endpointText(m.ReadEndpoint)} />,
					},
				]}
			/>
			<CacheTags scope={scope} arn={g.ARN} enabled={enabled} />
		</SpaceBetween>
	);
}

function CreateGroup({
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
	// Memcached cannot join a replication group.
	const engines = engineNames(options.versions).filter(
		(e) => e !== "memcached",
	);
	const [id, setId] = useState(""),
		[description, setDescription] = useState(""),
		[engine, setEngine] = useState(
			engines.includes("redis") ? "redis" : (engines[0] ?? ""),
		),
		[version, setVersion] = useState(
			() => versionsFor(options.versions, engine).at(-1) ?? "",
		),
		[nodeType, setNodeType] = useState(nodeTypes[0]),
		[replicas, setReplicas] = useState("1"),
		[failover, setFailover] = useState(true),
		[port, setPort] = useState(String(defaultPort(engine))),
		[subnet, setSubnet] = useState(""),
		[parameter, setParameter] = useState(""),
		[groups, setGroups] = useState<string[]>([]),
		[tags, setTags] = useState("{}");
	const families = new Set(
		familiesFor(options.versions.filter((v) => v.Engine === engine)),
	);
	return (
		<ActionDialog
			title="Create replication group"
			submit="Create replication group"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = groupCreateInput({
					id,
					description,
					engine,
					version,
					nodeType,
					replicas,
					failover,
					port,
					subnetGroup: subnet,
					parameterGroup: parameter,
					securityGroups: groups,
					tags: stringMap(tags, "Tags"),
				});
				return [
					{
						label: "Create replication group",
						run: () => ecCall(scope, "CreateReplicationGroup", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					Single-shard groups only: one primary and up to five replicas.
				</Alert>
				<FormField
					label="Replication group ID"
					description="Lowercase letters, numbers and hyphens, starting with a letter."
				>
					<Input
						ariaLabel="Replication group ID"
						value={id}
						onChange={({ detail }) => setId(detail.value)}
					/>
				</FormField>
				<FormField label="Description">
					<Input
						ariaLabel="Description"
						value={description}
						onChange={({ detail }) => setDescription(detail.value)}
					/>
				</FormField>
				<SelectField
					label="Engine"
					value={engine}
					options={engines}
					onChange={(v) => {
						setEngine(v);
						setVersion(versionsFor(options.versions, v).at(-1) ?? "");
						setParameter("");
					}}
				/>
				<SelectField
					label="Engine version"
					value={version}
					options={versionsFor(options.versions, engine)}
					onChange={setVersion}
					empty="Choose an engine first"
				/>
				<SelectField
					label="Node type"
					value={nodeType}
					options={nodeTypes}
					onChange={setNodeType}
				/>
				<FormField label="Replicas" description="0 to 5 replicas per primary.">
					<Input
						ariaLabel="Replicas"
						inputMode="numeric"
						value={replicas}
						onChange={({ detail }) => setReplicas(detail.value)}
					/>
				</FormField>
				<Checkbox
					checked={failover}
					onChange={({ detail }) => setFailover(detail.checked)}
				>
					Automatic failover
				</Checkbox>
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

function ModifyGroup({
	scope,
	group,
	options,
	onClose,
	onSaved,
}: {
	scope: ElastiCacheScope;
	group: ReplicationGroup;
	options: CacheOptions;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [description, setDescription] = useState(group.Description ?? ""),
		[failover, setFailover] = useState(group.AutomaticFailover === "enabled"),
		[parameter, setParameter] = useState(""),
		[primary, setPrimary] = useState("");
	const families = new Set(
		familiesFor(
			options.versions.filter((v) => v.Engine === (group.Engine ?? "redis")),
		),
	);
	return (
		<ActionDialog
			title={`Modify ${group.ReplicationGroupId}`}
			submit="Save changes"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!description.trim()) throw new Error("Description is required.");
				if (failover && replicaCount(group) < 1)
					throw new Error("Automatic failover requires at least one replica.");
				return [
					{
						label: "Modify replication group",
						run: () =>
							ecCall(scope, "ModifyReplicationGroup", {
								ReplicationGroupId: group.ReplicationGroupId,
								ReplicationGroupDescription: description.trim(),
								AutomaticFailoverEnabled: failover,
								ApplyImmediately: true,
								...(parameter ? { CacheParameterGroupName: parameter } : {}),
								...(primary ? { PrimaryClusterId: primary } : {}),
							}),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					Node type and engine version cannot be changed after creation.
				</Alert>
				<FormField label="Description">
					<Input
						ariaLabel="Description"
						value={description}
						onChange={({ detail }) => setDescription(detail.value)}
					/>
				</FormField>
				<Checkbox
					checked={failover}
					onChange={({ detail }) => setFailover(detail.checked)}
				>
					Automatic failover
				</Checkbox>
				<SelectField
					label="Promote primary"
					description="Leave empty to keep the current primary."
					value={primary}
					options={group.MemberClusters ?? []}
					onChange={setPrimary}
				/>
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

function ScaleGroup({
	scope,
	group,
	onClose,
	onSaved,
}: {
	scope: ElastiCacheScope;
	group: ReplicationGroup;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [count, setCount] = useState(String(replicaCount(group)));
	return (
		<ActionDialog
			title={`Scale ${group.ReplicationGroupId}`}
			submit="Apply replica count"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const step = replicaCountStep(replicaCount(group), count);
				return [
					{
						label: "Change replica count",
						run: () =>
							ecCall(scope, step.operation, {
								ReplicationGroupId: group.ReplicationGroupId,
								...step.input,
							}),
					},
				];
			}}
		>
			<FormField
				label="Replicas"
				description={`Currently ${replicaCount(group)}. A group has at most five replicas.`}
			>
				<Input
					ariaLabel="Replicas"
					inputMode="numeric"
					value={count}
					onChange={({ detail }) => setCount(detail.value)}
				/>
			</FormField>
		</ActionDialog>
	);
}

function DeleteGroup({
	scope,
	group,
	onClose,
	onSaved,
}: {
	scope: ElastiCacheScope;
	group: ReplicationGroup;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [retain, setRetain] = useState(false);
	return (
		<ActionDialog
			title={`Delete ${group.ReplicationGroupId}`}
			submit="Delete replication group"
			confirmation={group.ReplicationGroupId}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "Delete replication group",
					run: () =>
						ecCall(scope, "DeleteReplicationGroup", {
							ReplicationGroupId: group.ReplicationGroupId,
							RetainPrimaryCluster: retain,
						}),
				},
			]}
		>
			<SpaceBetween size="m">
				<Alert type="warning">
					Deleting the group removes its replicas and data. Snapshots are not
					supported, so no final snapshot is taken.
				</Alert>
				<Checkbox
					checked={retain}
					onChange={({ detail }) => setRetain(detail.checked)}
				>
					Retain the primary cluster as a standalone cluster
				</Checkbox>
			</SpaceBetween>
		</ActionDialog>
	);
}
