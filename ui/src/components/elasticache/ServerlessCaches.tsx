import {
	Alert,
	Button,
	FormField,
	Input,
	KeyValuePairs,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { stringMap } from "#/lib/consoleActions";
import {
	cliCommand,
	type ElastiCacheScope,
	ecCall,
	ecDate,
	endpointText,
	engineNames,
	type ServerlessCache,
	serverlessPages,
} from "#/lib/elasticache";
import { hasPending, serverlessCreateInput } from "#/lib/elasticacheHelpers";
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

export function ServerlessCaches({
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
		[selectedName, setSelectedName] = useState("");
	const poll = usePoll(
		async () => {
			const items = enabled ? await serverlessPages(scope) : [];
			setPending(hasPending(items.map((c) => c.Status)));
			return items;
		},
		pending ? 3000 : 0,
	);
	const items = poll.data ?? [],
		selected = items.find((c) => c.ServerlessCacheName === selectedName);
	const changed = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	return (
		<>
			<CacheList
				title="Serverless caches"
				items={items}
				trackBy="ServerlessCacheName"
				loading={poll.loading}
				error={poll.error}
				emptyText="Choose Create serverless cache to add one."
				onRefresh={poll.reload}
				selected={selected}
				onSelect={(c) => setSelectedName(c?.ServerlessCacheName ?? "")}
				actions={
					<>
						<Button
							disabled={!enabled || !selected}
							onClick={() => setDialog("modify")}
						>
							Modify
						</Button>
						<Button
							disabled={!enabled || !selected}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog("create")}
						>
							Create serverless cache
						</Button>
					</>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						sortingField: "ServerlessCacheName",
						isRowHeader: true,
						cell: (c) => c.ServerlessCacheName,
					},
					{
						id: "engine",
						header: "Engine and version",
						cell: (c) =>
							`${c.Engine} ${c.FullEngineVersion ?? c.MajorEngineVersion ?? ""}`,
					},
					{
						id: "status",
						header: "Status",
						cell: (c) => <CacheStatus status={c.Status} />,
					},
					{
						id: "endpoint",
						header: "Endpoint",
						cell: (c) => endpointText(c.Endpoint) || "—",
					},
					{
						id: "created",
						header: "Created",
						cell: (c) => ecDate(c.CreateTime),
					},
				]}
				detail={
					selected && (
						<SpaceBetween size="l">
							<Details cache={selected} />
							<CacheTags scope={scope} arn={selected.ARN} enabled={enabled} />
						</SpaceBetween>
					)
				}
			/>
			{dialog === "create" && (
				<CreateServerless
					scope={scope}
					options={options}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "modify" && selected && (
				<ModifyServerless
					scope={scope}
					cache={selected}
					options={options}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "delete" && selected && (
				<ActionDialog
					title={`Delete ${selected.ServerlessCacheName}`}
					submit="Delete serverless cache"
					confirmation={selected.ServerlessCacheName}
					onClose={() => setDialog("")}
					onSaved={() => {
						setSelectedName("");
						changed();
					}}
					plan={async () => [
						{
							label: "Delete serverless cache",
							run: () =>
								ecCall(scope, "DeleteServerlessCache", {
									ServerlessCacheName: selected.ServerlessCacheName,
								}),
						},
					]}
				>
					<Alert type="warning">
						Deleting the cache removes its data. Snapshots are not supported, so
						no final snapshot is taken.
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}

function Details({ cache: c }: { cache: ServerlessCache }) {
	const endpoint = endpointText(c.Endpoint),
		command = cliCommand(c.Engine, endpoint);
	return (
		<DetailBox title={c.ServerlessCacheName}>
			<KeyValuePairs
				columns={3}
				items={[
					{ label: "Status", value: <CacheStatus status={c.Status} /> },
					{ label: "Description", value: c.Description || "—" },
					{ label: "Subnets", value: c.SubnetIds?.join(", ") || "—" },
					{ label: "Endpoint", value: <EndpointValue value={endpoint} /> },
					{
						label: "Reader endpoint",
						value: <EndpointValue value={endpointText(c.ReaderEndpoint)} />,
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
								{c.ARN}
								{copyText(c.ARN)}
							</SpaceBetween>
						),
					},
				]}
			/>
		</DetailBox>
	);
}

function CreateServerless({
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
	const [name, setName] = useState(""),
		[engine, setEngine] = useState(
			engines.includes("valkey") ? "valkey" : (engines[0] ?? ""),
		),
		[description, setDescription] = useState(""),
		[subnets, setSubnets] = useState<string[]>([]),
		[groups, setGroups] = useState<string[]>([]),
		[tags, setTags] = useState("{}");
	return (
		<ActionDialog
			title="Create serverless cache"
			submit="Create serverless cache"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = serverlessCreateInput({
					name,
					engine,
					description,
					subnets,
					securityGroups: groups,
					tags: stringMap(tags, "Tags"),
				});
				return [
					{
						label: "Create serverless cache",
						run: () => ecCall(scope, "CreateServerlessCache", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<FormField
					label="Name"
					description="Lowercase letters, numbers and hyphens, starting with a letter."
				>
					<Input
						ariaLabel="Serverless cache name"
						value={name}
						onChange={({ detail }) => setName(detail.value)}
					/>
				</FormField>
				<SelectField
					label="Engine"
					value={engine}
					options={engines}
					onChange={setEngine}
				/>
				<FormField label="Description">
					<Input
						ariaLabel="Description"
						value={description}
						onChange={({ detail }) => setDescription(detail.value)}
					/>
				</FormField>
				<MultiField
					label="Subnets"
					values={subnets}
					options={options.subnets}
					onChange={setSubnets}
					empty="No EC2 subnets"
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

function ModifyServerless({
	scope,
	cache,
	options,
	onClose,
	onSaved,
}: {
	scope: ElastiCacheScope;
	cache: ServerlessCache;
	options: CacheOptions;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [description, setDescription] = useState(cache.Description ?? ""),
		[groups, setGroups] = useState<string[]>(cache.SecurityGroupIds ?? []);
	return (
		<ActionDialog
			title={`Modify ${cache.ServerlessCacheName}`}
			submit="Save changes"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "Modify serverless cache",
					run: () =>
						ecCall(scope, "ModifyServerlessCache", {
							ServerlessCacheName: cache.ServerlessCacheName,
							Description: description,
							...(groups.length ? { SecurityGroupIds: groups } : {}),
						}),
				},
			]}
		>
			<SpaceBetween size="m">
				<FormField label="Description">
					<Input
						ariaLabel="Description"
						value={description}
						onChange={({ detail }) => setDescription(detail.value)}
					/>
				</FormField>
				<MultiField
					label="Security groups"
					values={groups}
					options={options.securityGroups}
					onChange={setGroups}
					empty="No EC2 security groups"
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
