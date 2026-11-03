import {
	Alert,
	Button,
	FormField,
	Input,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { stringMap } from "#/lib/consoleActions";
import {
	type CacheEvent,
	type CacheParameter,
	type ElastiCacheScope,
	ecCall,
	ecDate,
	ecPages,
	familiesFor,
	isDefaultGroup,
	type ParameterGroup,
	type SubnetGroup,
} from "#/lib/elasticache";
import { validateGroupName } from "#/lib/elasticacheHelpers";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";
import {
	CacheList,
	type CacheOptions,
	CacheTags,
	DetailBox,
	MultiField,
	SelectField,
} from "./CacheKit";
import { TagsField } from "./TagsField";

export function SubnetGroups({
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
	const [dialog, setDialog] = useState(""),
		[selectedName, setSelectedName] = useState(""),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[subnets, setSubnets] = useState<string[]>([]),
		[tags, setTags] = useState("{}");
	const poll = usePoll(
		() =>
			enabled
				? ecPages<SubnetGroup>(
						scope,
						"DescribeCacheSubnetGroups",
						"CacheSubnetGroups",
					)
				: Promise.resolve([] as SubnetGroup[]),
		0,
	);
	const items = poll.data ?? [],
		selected = items.find((g) => g.CacheSubnetGroupName === selectedName);
	const changed = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	return (
		<>
			<CacheList
				title="Subnet groups"
				items={items}
				trackBy="CacheSubnetGroupName"
				loading={poll.loading}
				error={poll.error}
				emptyText="Choose Create subnet group to add one."
				onRefresh={poll.reload}
				selected={selected}
				onSelect={(g) => setSelectedName(g?.CacheSubnetGroupName ?? "")}
				actions={
					<>
						<Button
							disabled={
								!enabled ||
								!selected ||
								selected.CacheSubnetGroupName === "default"
							}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => {
								setName("");
								setDescription("");
								setSubnets([]);
								setTags("{}");
								setDialog("create");
							}}
						>
							Create subnet group
						</Button>
					</>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						sortingField: "CacheSubnetGroupName",
						isRowHeader: true,
						cell: (g) => g.CacheSubnetGroupName,
					},
					{
						id: "description",
						header: "Description",
						cell: (g) => g.CacheSubnetGroupDescription,
					},
					{ id: "vpc", header: "VPC", cell: (g) => g.VpcId ?? "—" },
					{
						id: "subnets",
						header: "Subnets",
						cell: (g) =>
							g.Subnets?.map((s) => s.SubnetIdentifier).join(", ") || "—",
					},
				]}
				detail={
					selected?.ARN && (
						<CacheTags scope={scope} arn={selected.ARN} enabled={enabled} />
					)
				}
			/>
			{dialog === "create" && (
				<ActionDialog
					title="Create subnet group"
					submit="Create subnet group"
					onClose={() => setDialog("")}
					onSaved={changed}
					plan={async () => {
						validateGroupName(name, "Subnet group name");
						if (!description.trim())
							throw new Error("Description is required.");
						if (!subnets.length) throw new Error("Choose at least one subnet.");
						const input = {
							CacheSubnetGroupName: name,
							CacheSubnetGroupDescription: description.trim(),
							SubnetIds: subnets,
							Tags: tagList(stringMap(tags, "Tags")),
						};
						return [
							{
								label: "Create subnet group",
								run: () => ecCall(scope, "CreateCacheSubnetGroup", input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<FormField label="Name">
							<Input
								ariaLabel="Subnet group name"
								value={name}
								onChange={({ detail }) => setName(detail.value)}
							/>
						</FormField>
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
							empty="No EC2 subnets; create a VPC subnet first"
						/>
						<TagsField value={tags} onChange={setTags} />
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && selected && (
				<ActionDialog
					title={`Delete ${selected.CacheSubnetGroupName}`}
					submit="Delete subnet group"
					confirmation={selected.CacheSubnetGroupName}
					onClose={() => setDialog("")}
					onSaved={() => {
						setSelectedName("");
						changed();
					}}
					plan={async () => [
						{
							label: "Delete subnet group",
							run: () =>
								ecCall(scope, "DeleteCacheSubnetGroup", {
									CacheSubnetGroupName: selected.CacheSubnetGroupName,
								}),
						},
					]}
				>
					<Alert type="warning">
						A subnet group used by a cache cannot be deleted.
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}

export function ParameterGroups({
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
	const [dialog, setDialog] = useState(""),
		[selectedName, setSelectedName] = useState(""),
		[name, setName] = useState(""),
		[family, setFamily] = useState(""),
		[description, setDescription] = useState(""),
		[tags, setTags] = useState("{}"),
		[parameter, setParameter] = useState(""),
		[value, setValue] = useState("");
	const poll = usePoll(
		() =>
			enabled
				? ecPages<ParameterGroup>(
						scope,
						"DescribeCacheParameterGroups",
						"CacheParameterGroups",
					)
				: Promise.resolve([] as ParameterGroup[]),
		0,
	);
	const items = poll.data ?? [],
		selected = items.find((g) => g.CacheParameterGroupName === selectedName);
	const parameters = usePoll(
		() =>
			enabled && selected
				? ecPages<CacheParameter>(
						scope,
						"DescribeCacheParameters",
						"Parameters",
						{ CacheParameterGroupName: selected.CacheParameterGroupName },
					)
				: Promise.resolve([] as CacheParameter[]),
		0,
	);
	const changed = () => {
		setDialog("");
		poll.reload();
		parameters.reload();
		onChanged();
	};
	const locked = !selected || isDefaultGroup(selected.CacheParameterGroupName);
	const modifiable = (parameters.data ?? []).filter((p) => p.IsModifiable);
	return (
		<>
			<CacheList
				title="Parameter groups"
				items={items}
				trackBy="CacheParameterGroupName"
				loading={poll.loading}
				error={poll.error}
				emptyText="Choose Create parameter group to add one."
				onRefresh={poll.reload}
				selected={selected}
				onSelect={(g) => setSelectedName(g?.CacheParameterGroupName ?? "")}
				actions={
					<>
						<Button
							disabled={!enabled || locked}
							onClick={() => {
								setParameter("");
								setValue("");
								setDialog("edit");
							}}
						>
							Edit parameter
						</Button>
						<Button
							disabled={!enabled || locked}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => {
								setName("");
								setFamily("");
								setDescription("");
								setTags("{}");
								setDialog("create");
							}}
						>
							Create parameter group
						</Button>
					</>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						sortingField: "CacheParameterGroupName",
						isRowHeader: true,
						cell: (g) => g.CacheParameterGroupName,
					},
					{
						id: "family",
						header: "Family",
						cell: (g) => g.CacheParameterGroupFamily ?? "—",
					},
					{
						id: "description",
						header: "Description",
						cell: (g) => g.Description,
					},
				]}
				detail={
					selected && (
						<SpaceBetween size="l">
							<Table
								variant="container"
								header={<>Parameters</>}
								loading={parameters.loading}
								items={parameters.data ?? []}
								trackBy="ParameterName"
								empty="No parameters."
								columnDefinitions={[
									{
										id: "name",
										header: "Parameter",
										cell: (p) => p.ParameterName,
									},
									{
										id: "value",
										header: "Value",
										cell: (p) => p.ParameterValue ?? "—",
									},
									{
										id: "source",
										header: "Source",
										cell: (p) => p.Source ?? "—",
									},
									{
										id: "modifiable",
										header: "Modifiable",
										cell: (p) => (p.IsModifiable ? "Yes" : "No"),
									},
								]}
							/>
							{selected.ARN && (
								<CacheTags scope={scope} arn={selected.ARN} enabled={enabled} />
							)}
						</SpaceBetween>
					)
				}
			/>
			{dialog === "create" && (
				<ActionDialog
					title="Create parameter group"
					submit="Create parameter group"
					onClose={() => setDialog("")}
					onSaved={changed}
					plan={async () => {
						validateGroupName(name, "Parameter group name");
						if (!family) throw new Error("Choose a parameter group family.");
						if (!description.trim())
							throw new Error("Description is required.");
						const input = {
							CacheParameterGroupName: name,
							CacheParameterGroupFamily: family,
							Description: description.trim(),
							Tags: tagList(stringMap(tags, "Tags")),
						};
						return [
							{
								label: "Create parameter group",
								run: () => ecCall(scope, "CreateCacheParameterGroup", input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<FormField label="Name">
							<Input
								ariaLabel="Parameter group name"
								value={name}
								onChange={({ detail }) => setName(detail.value)}
							/>
						</FormField>
						<SelectField
							label="Family"
							value={family}
							options={familiesFor(options.versions)}
							onChange={setFamily}
						/>
						<FormField label="Description">
							<Input
								ariaLabel="Description"
								value={description}
								onChange={({ detail }) => setDescription(detail.value)}
							/>
						</FormField>
						<TagsField value={tags} onChange={setTags} />
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "edit" && selected && (
				<ActionDialog
					title={`Edit ${selected.CacheParameterGroupName}`}
					submit="Save parameter"
					onClose={() => setDialog("")}
					onSaved={changed}
					plan={async () => {
						if (!parameter) throw new Error("Choose a parameter.");
						return [
							{
								label: "Modify parameter group",
								run: () =>
									ecCall(scope, "ModifyCacheParameterGroup", {
										CacheParameterGroupName: selected.CacheParameterGroupName,
										ParameterNameValues: [
											{ ParameterName: parameter, ParameterValue: value },
										],
									}),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<SelectField
							label="Parameter"
							value={parameter}
							options={modifiable.map((p) => p.ParameterName)}
							onChange={(v) => {
								setParameter(v);
								setValue(
									modifiable.find((p) => p.ParameterName === v)
										?.ParameterValue ?? "",
								);
							}}
							empty="No modifiable parameters"
						/>
						<FormField
							label="Value"
							description={
								modifiable.find((p) => p.ParameterName === parameter)
									?.AllowedValues
									? `Allowed: ${modifiable.find((p) => p.ParameterName === parameter)?.AllowedValues}`
									: undefined
							}
						>
							<Input
								ariaLabel="Parameter value"
								value={value}
								onChange={({ detail }) => setValue(detail.value)}
							/>
						</FormField>
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && selected && (
				<ActionDialog
					title={`Delete ${selected.CacheParameterGroupName}`}
					submit="Delete parameter group"
					confirmation={selected.CacheParameterGroupName}
					onClose={() => setDialog("")}
					onSaved={() => {
						setSelectedName("");
						changed();
					}}
					plan={async () => [
						{
							label: "Delete parameter group",
							run: () =>
								ecCall(scope, "DeleteCacheParameterGroup", {
									CacheParameterGroupName: selected.CacheParameterGroupName,
								}),
						},
					]}
				>
					<Alert type="warning">
						A parameter group used by a cache cannot be deleted.
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}

export function CacheEvents({
	scope,
	enabled,
}: {
	scope: ElastiCacheScope;
	enabled: boolean;
}) {
	const poll = usePoll(
		() =>
			enabled
				? ecPages<CacheEvent>(scope, "DescribeEvents", "Events", {
						Duration: 20160,
					})
				: Promise.resolve([] as CacheEvent[]),
		10000,
	);
	return (
		<DetailBox title="Events (last 14 days)">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				variant="embedded"
				loading={poll.loading}
				items={poll.data ?? []}
				empty="No events yet. Creating or changing a cache records one."
				columnDefinitions={[
					{ id: "date", header: "Date", cell: (e) => ecDate(e.Date) },
					{ id: "type", header: "Source type", cell: (e) => e.SourceType },
					{ id: "source", header: "Source", cell: (e) => e.SourceIdentifier },
					{ id: "message", header: "Message", cell: (e) => e.Message },
				]}
			/>
		</DetailBox>
	);
}
