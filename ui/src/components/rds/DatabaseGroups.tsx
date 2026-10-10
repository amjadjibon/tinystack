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
	type DBGroup,
	engines,
	type RDSScope,
	rdsCall,
	rdsPages,
	rdsTags,
	validateName,
} from "#/lib/rds";
import { usePoll } from "#/lib/usePoll";
export function DatabaseGroups({
	scope,
	enabled,
	onChanged,
}: {
	scope: RDSScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const subnet = scope.section === "subnets",
		kind = subnet ? "Subnet" : "Parameter",
		nameKey = subnet ? "DBSubnetGroupName" : "DBParameterGroupName";
	const [dialog, setDialog] = useState(""),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[family, setFamily] = useState("postgres16"),
		[ids, setIds] = useState(""),
		[tags, setTags] = useState("{}");
	const poll = usePoll(() =>
		enabled
			? rdsPages<DBGroup>(scope, `DescribeDB${kind}Groups`, `DB${kind}Groups`)
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
			selection: { trackBy: nameKey },
		});
	const selected = collectionProps.selectedItems?.[0],
		selectedName = selected?.[nameKey];
	const changed = () => {
		poll.reload();
		onChanged();
	};
	const field = (label: string, value: string, update: (v: string) => void) => (
		<FormField label={label}>
			<Input
				ariaLabel={label}
				value={value}
				onChange={({ detail }) => update(detail.value)}
			/>
		</FormField>
	);
	return (
		<SpaceBetween size="l">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
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
				trackBy={nameKey}
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
									disabled={!enabled || !selectedName}
									onClick={() => {
										setName(selectedName ?? "");
										setDialog("Delete");
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
										setIds("");
										setTags("{}");
										setDialog("Create");
									}}
								>
									Create {kind.toLowerCase()} group
								</Button>
							</SpaceBetween>
						}
					>
						{kind} groups
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
						cell: (g) => g[nameKey],
						sortingField: nameKey,
						isRowHeader: true,
					},
					{
						id: "description",
						header: "Description",
						cell: (g) => g.DBSubnetGroupDescription ?? g.Description,
					},
					...(subnet
						? [
								{ id: "vpc", header: "VPC", cell: (g: DBGroup) => g.VpcId },
								{
									id: "subnets",
									header: "Subnets",
									cell: (g: DBGroup) =>
										g.Subnets?.map((s) => s.SubnetIdentifier).join(", "),
								},
							]
						: [
								{
									id: "family",
									header: "Family",
									cell: (g: DBGroup) => g.DBParameterGroupFamily,
								},
							]),
				]}
			/>
			{dialog && (
				<ActionDialog
					title={`${dialog} ${kind.toLowerCase()} group`}
					submit={`${dialog} group`}
					confirmation={dialog === "Delete" ? name : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						changed();
					}}
					plan={async () => {
						if (dialog === "Delete")
							return [
								{
									label: "Delete group",
									run: () =>
										rdsCall(scope, `DeleteDB${kind}Group`, {
											[nameKey]: name,
										}),
								},
							];
						validateName(name, 255);
						if (!description.trim())
							throw new Error("Description is required.");
						const subnetIDs = ids.split(/[\s,]+/).filter(Boolean);
						if (
							subnet &&
							(!subnetIDs.length ||
								new Set(subnetIDs).size !== subnetIDs.length)
						)
							throw new Error(
								"Enter unique existing EC2 subnet IDs in one VPC.",
							);
						return [
							{
								label: "Create group",
								run: () =>
									rdsCall(scope, `CreateDB${kind}Group`, {
										[nameKey]: name,
										...(subnet
											? {
													DBSubnetGroupDescription: description,
													SubnetIds: subnetIDs,
												}
											: {
													Description: description,
													DBParameterGroupFamily: family,
												}),
										Tags: rdsTags(stringMap(tags, "Tags")),
									}),
							},
						];
					}}
				>
					{dialog === "Delete" ? (
						<Alert type="warning">
							Delete {name}? Groups attached to a database cannot be deleted.
						</Alert>
					) : (
						<SpaceBetween size="m">
							{field("Group name", name, setName)}
							{field("Description", description, setDescription)}
							{subnet ? (
								<>
									{field("Subnet IDs (comma separated)", ids, setIds)}
									<Alert type="info">
										Use existing EC2 subnets in the selected account and Region,
										all in one VPC.
									</Alert>
								</>
							) : (
								<>
									<FormField label="Parameter group family">
										<Select
											selectedOption={{ label: family, value: family }}
											options={engines.flatMap((e) =>
												e.versions.map((v) => ({
													label: e.value + v,
													value: e.value + v,
												})),
											)}
											onChange={({ detail }) =>
												setFamily(detail.selectedOption.value ?? "")
											}
										/>
									</FormField>
									<Alert type="info">
										Parameter groups store metadata. Engine settings are not
										applied.
									</Alert>
								</>
							)}
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
