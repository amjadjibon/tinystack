import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	FormField,
	Header,
	Input,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { stringMap } from "#/lib/consoleActions";
import {
	clusterParameterFamilies,
	type DBGroup,
	type DBParameter,
	parameterFamilies,
	type RDSScope,
	rdsCall,
	rdsPages,
	validateName,
} from "#/lib/rds";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";

type Kind = { cluster: boolean; prefix: string; nameKey: keyof DBGroup };

function groupKind(cluster: boolean): Kind {
	return cluster
		? { cluster, prefix: "DBCluster", nameKey: "DBClusterParameterGroupName" }
		: { cluster, prefix: "DB", nameKey: "DBParameterGroupName" };
}

function groupCall(
	scope: RDSScope,
	kind: Kind,
	operation: string,
	group: string,
	input: Record<string, unknown> = {},
) {
	return rdsCall(scope, `${operation}${kind.prefix}ParameterGroup`, {
		[kind.nameKey]: group,
		...input,
	});
}

function choose(
	label: string,
	current: string,
	options: string[],
	update: (v: string) => void,
) {
	return (
		<FormField label={label}>
			<Select
				ariaLabel={label}
				selectedOption={{ label: current, value: current }}
				options={options.map((o) => ({ label: o, value: o }))}
				onChange={({ detail }) => update(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}

function text(label: string, value: string, update: (v: string) => void) {
	return (
		<FormField label={label}>
			<Input
				ariaLabel={label}
				value={value}
				onChange={({ detail }) => update(detail.value)}
			/>
		</FormField>
	);
}

/** ParameterGroups lists DB or DB cluster parameter groups and shows the selected group's parameters. */
export function ParameterGroups({
	scope,
	enabled,
	cluster,
	onChanged,
}: {
	scope: RDSScope;
	enabled: boolean;
	cluster: boolean;
	onChanged: () => void;
}) {
	const kind = groupKind(cluster),
		families = cluster ? clusterParameterFamilies : parameterFamilies,
		noun = cluster ? "cluster parameter group" : "parameter group";
	const [dialog, setDialog] = useState<"" | "create" | "delete">(""),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[family, setFamily] = useState(families[0]),
		[tags, setTags] = useState("{}");
	const poll = usePoll(() =>
		enabled
			? rdsPages<DBGroup>(
					scope,
					`Describe${kind.prefix}ParameterGroups`,
					`${kind.prefix}ParameterGroups`,
				)
			: Promise.resolve([] as DBGroup[]),
	);
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(poll.data ?? [], {
			filtering: {
				empty: <Box>No groups.</Box>,
				noMatch: <Box>No matching groups.</Box>,
			},
			pagination: { pageSize: 20 },
			sorting: {},
			selection: { trackBy: kind.nameKey },
		});
	const group = String(
		collectionProps.selectedItems?.[0]?.[kind.nameKey] ?? "",
	);
	const changed = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!enabled && (
				<Alert type="info">
					Use the resource inventory and signed CLI or SDK requests for group
					metadata in verify mode.
				</Alert>
			)}
			<Table
				{...collectionProps}
				items={items}
				selectionType="single"
				trackBy={kind.nameKey}
				loading={poll.loading}
				header={
					<Header
						variant="h1"
						counter={`(${poll.data?.length ?? 0})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh groups"
									onClick={poll.reload}
								/>
								<Button
									disabled={!enabled || !group}
									onClick={() => {
										setName(group);
										setDialog("delete");
									}}
								>
									Delete
								</Button>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => {
										setName("");
										setDescription("");
										setTags("{}");
										setDialog("create");
									}}
								>
									Create {noun}
								</Button>
							</SpaceBetween>
						}
					>
						{cluster ? "Cluster parameter groups" : "Parameter groups"}
					</Header>
				}
				filter={
					<TextFilter {...filterProps} filteringPlaceholder="Find groups" />
				}
				pagination={<Pagination {...paginationProps} />}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						cell: (g) => g[kind.nameKey] as string,
						sortingField: kind.nameKey,
						isRowHeader: true,
					},
					{
						id: "family",
						header: "Family",
						cell: (g) => g.DBParameterGroupFamily,
					},
					{
						id: "description",
						header: "Description",
						cell: (g) => g.Description,
					},
				]}
			/>
			{!!group && (
				<GroupParameters
					key={group}
					{...{ scope, enabled, kind, group }}
					onChanged={onChanged}
				/>
			)}
			{!!dialog && (
				<ActionDialog
					title={`${dialog === "create" ? "Create" : "Delete"} ${noun}`}
					submit={`${dialog === "create" ? "Create" : "Delete"} group`}
					confirmation={dialog === "delete" ? name : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						changed();
					}}
					plan={async () => {
						if (dialog === "delete")
							return [
								{
									label: "Delete group",
									run: () => groupCall(scope, kind, "Delete", name),
								},
							];
						validateName(name, 255);
						if (!description.trim())
							throw new Error("Description is required.");
						return [
							{
								label: "Create group",
								run: () =>
									groupCall(scope, kind, "Create", name, {
										Description: description,
										DBParameterGroupFamily: family,
										Tags: tagList(stringMap(tags, "Tags")),
									}),
							},
						];
					}}
				>
					{dialog === "delete" ? (
						<Alert type="warning">
							Delete {name}? Groups attached to a database cannot be deleted.
						</Alert>
					) : (
						<SpaceBetween size="m">
							{text("Group name", name, setName)}
							{text("Description", description, setDescription)}
							{choose("Parameter group family", family, families, setFamily)}
							<FormField label="Tags (JSON)">
								<Textarea
									ariaLabel="Group tags"
									value={tags}
									onChange={({ detail }) => setTags(detail.value)}
								/>
							</FormField>
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

/** GroupParameters lists one group's parameters and edits or resets them. */
function GroupParameters({
	scope,
	enabled,
	kind,
	group,
	onChanged,
}: {
	scope: RDSScope;
	enabled: boolean;
	kind: Kind;
	group: string;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<"" | "edit" | "reset" | "reset-all">(""),
		[value, setValue] = useState(""),
		[method, setMethod] = useState("pending-reboot");
	const poll = usePoll(
		() =>
			enabled
				? rdsPages<DBParameter>(
						scope,
						`Describe${kind.prefix}Parameters`,
						"Parameters",
						{ [kind.nameKey]: group },
					)
				: Promise.resolve([] as DBParameter[]),
		0,
	);
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(poll.data ?? [], {
			filtering: {
				empty: <Box>No parameters.</Box>,
				noMatch: <Box>No matching parameters.</Box>,
			},
			pagination: { pageSize: 50 },
			sorting: {},
			selection: { trackBy: "ParameterName" },
		});
	const chosen = collectionProps.selectedItems ?? [],
		parameter = chosen.length === 1 ? chosen[0] : undefined,
		immutable = group.toLowerCase().startsWith("default."),
		choices = parameter && valueChoices(parameter);
	const steps = () => {
		if (dialog === "reset-all")
			return [
				{
					label: "Reset all parameters",
					run: () =>
						groupCall(scope, kind, "Reset", group, {
							ResetAllParameters: true,
						}),
				},
			];
		if (dialog === "reset")
			return [
				{
					label: "Reset parameters",
					run: () =>
						groupCall(scope, kind, "Reset", group, {
							Parameters: chosen.map((p) => ({
								ParameterName: p.ParameterName,
								ApplyMethod:
									p.ApplyType === "static" ? "pending-reboot" : "immediate",
							})),
						}),
				},
			];
		if (!parameter) throw new Error("Choose one parameter.");
		return [
			{
				label: "Modify parameter",
				run: () =>
					groupCall(scope, kind, "Modify", group, {
						Parameters: [
							{
								ParameterName: parameter.ParameterName,
								ParameterValue: value,
								ApplyMethod: method,
							},
						],
					}),
			},
		];
	};
	return (
		<>
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				{...collectionProps}
				items={items}
				selectionType="multi"
				trackBy="ParameterName"
				loading={poll.loading}
				header={
					<Header
						variant="h2"
						counter={`(${poll.data?.length ?? 0})`}
						description={
							immutable
								? "Default groups are read only. Create a group to change values."
								: "Static values apply when the database restarts; dynamic values can apply immediately."
						}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh parameters"
									onClick={poll.reload}
								/>
								<Button
									disabled={!enabled || immutable || !chosen.length}
									onClick={() => setDialog("reset")}
								>
									Reset
								</Button>
								<Button
									disabled={!enabled || immutable}
									onClick={() => setDialog("reset-all")}
								>
									Reset all
								</Button>
								<Button
									variant="primary"
									disabled={
										!enabled ||
										immutable ||
										!parameter ||
										parameter.IsModifiable === false
									}
									onClick={() => {
										setValue(parameter?.ParameterValue ?? "");
										setMethod(
											parameter?.ApplyType === "static"
												? "pending-reboot"
												: "immediate",
										);
										setDialog("edit");
									}}
								>
									Edit
								</Button>
							</SpaceBetween>
						}
					>
						Parameters: {group}
					</Header>
				}
				filter={
					<TextFilter {...filterProps} filteringPlaceholder="Find parameters" />
				}
				pagination={<Pagination {...paginationProps} />}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						cell: (p) => p.ParameterName,
						sortingField: "ParameterName",
						isRowHeader: true,
					},
					{ id: "value", header: "Value", cell: (p) => p.ParameterValue },
					{
						id: "allowed",
						header: "Allowed values",
						cell: (p) => p.AllowedValues,
					},
					{ id: "source", header: "Source", cell: (p) => p.Source },
					{ id: "apply", header: "Apply type", cell: (p) => p.ApplyType },
					{ id: "type", header: "Data type", cell: (p) => p.DataType },
					{
						id: "modifiable",
						header: "Modifiable",
						cell: (p) => (p.IsModifiable === false ? "No" : "Yes"),
					},
				]}
			/>
			{!!dialog && (
				<ActionDialog
					title={
						dialog === "edit"
							? `Edit ${parameter?.ParameterName}`
							: `Reset parameters in ${group}`
					}
					submit={dialog === "edit" ? "Save" : "Reset"}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						poll.reload();
						onChanged();
					}}
					plan={async () => steps()}
				>
					{dialog === "reset" && (
						<Alert type="info">
							Reset {chosen.map((p) => p.ParameterName).join(", ")} to the
							engine defaults.
						</Alert>
					)}
					{dialog === "reset-all" && (
						<Alert type="warning">
							Reset every parameter in {group} to the engine defaults.
						</Alert>
					)}
					{dialog === "edit" && parameter && (
						<SpaceBetween size="m">
							{choices ? (
								choose("Value", value, choices, setValue)
							) : (
								<FormField
									label="Value"
									description={
										parameter.AllowedValues
											? `Allowed: ${parameter.AllowedValues}`
											: undefined
									}
								>
									<Input
										ariaLabel="Value"
										value={value}
										onChange={({ detail }) => setValue(detail.value)}
									/>
								</FormField>
							)}
							{choose(
								"Apply method",
								method,
								parameter.ApplyType === "static"
									? ["pending-reboot"]
									: ["immediate", "pending-reboot"],
								setMethod,
							)}
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</>
	);
}

/** valueChoices returns an enumerated parameter's allowed values, or undefined for ranges and free text. */
function valueChoices(p: DBParameter) {
	if (!p.AllowedValues || p.DataType === "integer" || p.DataType === "float")
		return undefined;
	return p.AllowedValues.split(",");
}
