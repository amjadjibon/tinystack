import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	Container,
	Header,
	Pagination,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type HostedZone,
	type Route53Scope,
	type Route53Tag,
	recordPages,
	route53Call,
} from "#/lib/route53";
import {
	bareZoneId,
	isProtectedRecord,
	type RecordSet,
	recordKey,
	recordTarget,
	relativeName,
} from "#/lib/route53Helpers";
import { usePoll } from "#/lib/usePoll";
import { DeleteRecordDialog, RecordDialog } from "./RecordForms";
import { DeleteZoneDialog, EditCommentDialog } from "./ZoneForms";

export function ZoneDetails({
	scope,
	zone,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: Route53Scope;
	zone: HostedZone;
	enabled: boolean;
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const id = bareZoneId(zone.Id);
	const records = usePoll(() => recordPages(scope, id), 0);
	const tags = usePoll(
		() =>
			route53Call<{ ResourceTagSet?: { Tags?: Route53Tag[] } }>(
				scope,
				"ListTagsForResource",
				{ ResourceType: "hostedzone", ResourceId: id },
			),
		0,
	);
	const [dialog, setDialog] = useState<
		"comment" | "delete" | "create" | "edit" | "remove" | null
	>(null);
	const [selected, setSelected] = useState<RecordSet[]>([]);
	const current = selected[0];
	const all = records.data ?? [];
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(all, {
			filtering: {
				empty: <Box>No record sets in this hosted zone.</Box>,
				noMatch: <Box>No matching record sets.</Box>,
				filteringFunction: (r, text) =>
					`${r.Name} ${r.Type} ${recordTarget(r)}`
						.toLowerCase()
						.includes(text.toLowerCase()),
			},
			sorting: { defaultState: { sortingColumn: { sortingField: "Name" } } },
			pagination: { pageSize: 20 },
		});
	const refresh = () => {
		records.reload();
		onChanged();
	};
	const protectedRecord = current && isProtectedRecord(current, zone.Name);
	const tagMap = Object.fromEntries(
		(tags.data?.ResourceTagSet?.Tags ?? []).map((t) => [t.Key, t.Value]),
	);
	return (
		<SpaceBetween size="l">
			<Container
				header={
					<Header
						variant="h1"
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!enabled}
									onClick={() => setDialog("comment")}
								>
									Edit comment
								</Button>
								<Button disabled={!enabled} onClick={() => setDialog("delete")}>
									Delete hosted zone
								</Button>
							</SpaceBetween>
						}
					>
						{zone.Name}
					</Header>
				}
			>
				<ColumnLayout columns={4} variant="text-grid">
					<div>
						<Box variant="awsui-key-label">Hosted zone ID</Box>
						<div>{id}</div>
					</div>
					<div>
						<Box variant="awsui-key-label">Type</Box>
						<div>{zone.Config?.PrivateZone ? "Private" : "Public"}</div>
					</div>
					<div>
						<Box variant="awsui-key-label">Record sets</Box>
						<div>{zone.ResourceRecordSetCount ?? all.length}</div>
					</div>
					<div>
						<Box variant="awsui-key-label">Comment</Box>
						<div>{zone.Config?.Comment || "—"}</div>
					</div>
				</ColumnLayout>
			</Container>
			{records.error && (
				<Alert type="error" header="Cannot load record sets">
					{records.error}
				</Alert>
			)}
			<Table
				{...collectionProps}
				items={items}
				trackBy={recordKey}
				loading={records.loading}
				loadingText="Loading record sets"
				selectionType="single"
				selectedItems={selected}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				header={
					<Header
						variant="h2"
						counter={`(${all.length})`}
						description="Create, edit and delete record sets in this hosted zone."
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh record sets"
									onClick={records.reload}
								/>
								<Button
									disabled={!enabled || !current}
									onClick={() => setDialog("edit")}
								>
									Edit record
								</Button>
								<Button
									disabled={!enabled || !current || !!protectedRecord}
									onClick={() => setDialog("remove")}
								>
									Delete record
								</Button>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => setDialog("create")}
								>
									Create record
								</Button>
							</SpaceBetween>
						}
					>
						Records
					</Header>
				}
				filter={
					<TextFilter {...filterProps} filteringPlaceholder="Find records" />
				}
				pagination={<Pagination {...paginationProps} />}
				empty={<Box>No record sets in this hosted zone.</Box>}
				columnDefinitions={[
					{
						id: "name",
						header: "Record name",
						cell: (r) => relativeName(r.Name, zone.Name),
						sortingField: "Name",
						isRowHeader: true,
					},
					{
						id: "type",
						header: "Type",
						cell: (r) => r.Type,
						sortingField: "Type",
					},
					{
						id: "ttl",
						header: "TTL",
						cell: (r) => (r.TTL === undefined ? "—" : r.TTL),
					},
					{
						id: "values",
						header: "Value / alias target",
						cell: (r) => (
							<span style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
								{recordTarget(r)}
							</span>
						),
					},
					{
						id: "routing",
						header: "Routing",
						cell: (r) => r.SetIdentifier ?? "Simple",
					},
				]}
			/>
			{protectedRecord && (
				<Alert type="info">
					The zone's own {current?.Type} record set cannot be deleted. You can
					edit it.
				</Alert>
			)}
			<ConsoleTags
				tags={tagMap}
				enabled={enabled}
				onChanged={() => tags.reload()}
				plan={(next, removed) => [
					{
						label: "Update tags",
						run: () =>
							route53Call(scope, "ChangeTagsForResource", {
								ResourceType: "hostedzone",
								ResourceId: id,
								...(Object.keys(next).length
									? {
											AddTags: Object.entries(next).map(([Key, Value]) => ({
												Key,
												Value,
											})),
										}
									: {}),
								...(removed.length ? { RemoveTagKeys: removed } : {}),
							}),
					},
				]}
			/>
			{dialog === "comment" && (
				<EditCommentDialog
					scope={scope}
					zone={zone}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						onChanged();
					}}
				/>
			)}
			{dialog === "delete" && (
				<DeleteZoneDialog
					scope={scope}
					zone={zone}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						onDeleted();
					}}
				/>
			)}
			{(dialog === "create" || (dialog === "edit" && current)) && (
				<RecordDialog
					scope={scope}
					zone={zone}
					record={dialog === "edit" ? current : undefined}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						setSelected([]);
						refresh();
					}}
				/>
			)}
			{dialog === "remove" && current && (
				<DeleteRecordDialog
					scope={scope}
					zone={zone}
					record={current}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						setSelected([]);
						refresh();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
