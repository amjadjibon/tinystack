import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	FormField,
	Header,
	Input,
	Pagination,
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
	type RDSScope,
	rdsCall,
	rdsPages,
	validateName,
} from "#/lib/rds";
import { tagList } from "#/lib/tags";
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
	const nameKey = "DBSubnetGroupName";
	const [dialog, setDialog] = useState(""),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[ids, setIds] = useState(""),
		[tags, setTags] = useState("{}");
	const poll = usePoll(() =>
		enabled
			? rdsPages<DBGroup>(scope, "DescribeDBSubnetGroups", "DBSubnetGroups")
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
									Create subnet group
								</Button>
							</SpaceBetween>
						}
					>
						Subnet groups
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
						cell: (g) => g.DBSubnetGroupDescription,
					},
					{ id: "vpc", header: "VPC", cell: (g) => g.VpcId },
					{
						id: "subnets",
						header: "Subnets",
						cell: (g) => g.Subnets?.map((s) => s.SubnetIdentifier).join(", "),
					},
				]}
			/>
			{!!dialog && (
				<ActionDialog
					title={`${dialog} subnet group`}
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
										rdsCall(scope, "DeleteDBSubnetGroup", {
											[nameKey]: name,
										}),
								},
							];
						validateName(name, 255);
						if (!description.trim())
							throw new Error("Description is required.");
						const subnetIDs = ids.split(/[\s,]+/).filter(Boolean);
						if (
							!subnetIDs.length ||
							new Set(subnetIDs).size !== subnetIDs.length
						)
							throw new Error(
								"Enter unique existing EC2 subnet IDs in one VPC.",
							);
						return [
							{
								label: "Create group",
								run: () =>
									rdsCall(scope, "CreateDBSubnetGroup", {
										[nameKey]: name,
										DBSubnetGroupDescription: description,
										SubnetIds: subnetIDs,
										Tags: tagList(stringMap(tags, "Tags")),
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
							{field("Subnet IDs (comma separated)", ids, setIds)}
							<Alert type="info">
								Use existing EC2 subnets in the selected account and Region, all
								in one VPC.
							</Alert>
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
