import {
	Alert,
	Box,
	Button,
	ButtonDropdown,
	Header,
	SpaceBetween,
	Table,
	Tabs,
} from "@cloudscape-design/components";
import { useState } from "react";
import {
	type DBCluster,
	type DBInstance,
	type DBSnapshot,
	hasPendingSnapshots,
	type RDSScope,
	rdsCall,
	rdsDate,
	rdsPages,
	validateName,
} from "#/lib/rds";
import { usePoll } from "#/lib/usePoll";
import { DatabaseStatus } from "./DatabaseDetails";
import { type RDSField, RDSOperationDialog } from "./RDSOperationDialog";
import { RDSTags } from "./RDSTags";
export function DatabaseSnapshots(props: {
	scope: RDSScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	return (
		<Tabs
			tabs={[
				{
					id: "instances",
					label: "Instance snapshots",
					content: <SnapshotList {...props} cluster={false} />,
				},
				{
					id: "clusters",
					label: "Cluster snapshots",
					content: <SnapshotList {...props} cluster />,
				},
			]}
		/>
	);
}
function SnapshotList({
	scope,
	enabled,
	onChanged,
	cluster,
}: {
	scope: RDSScope;
	enabled: boolean;
	onChanged: () => void;
	cluster: boolean;
}) {
	const kind = cluster ? "DBClusterSnapshot" : "DBSnapshot",
		field = `${kind}Identifier`,
		targetField = `Target${field}`,
		restoreField = cluster ? "DBClusterIdentifier" : "DBInstanceIdentifier";
	const [pending, setPending] = useState(false);
	const poll = usePoll(
		async () => {
			const items = enabled
				? await rdsPages<DBSnapshot>(scope, `Describe${kind}s`, `${kind}s`)
				: [];
			setPending(hasPendingSnapshots(items));
			return items;
		},
		pending ? 3000 : 0,
	);
	const sources = usePoll(async () => {
		if (!enabled) return [];
		return cluster
			? (
					await rdsPages<DBCluster>(scope, "DescribeDBClusters", "DBClusters")
				).map((c) => c.DBClusterIdentifier)
			: (
					await rdsPages<DBInstance>(
						scope,
						"DescribeDBInstances",
						"DBInstances",
					)
				).map((i) => i.DBInstanceIdentifier);
	}, 0);
	const [selected, setSelected] = useState(""),
		[action, setAction] = useState("");
	const identifier = (s: DBSnapshot) =>
		(cluster ? s.DBClusterSnapshotIdentifier : s.DBSnapshotIdentifier) ?? "";
	const snapshot = poll.data?.find((s) => identifier(s) === selected);
	const snapshotAvailable = enabled && snapshot?.Status === "available";
	const changed = () => {
		poll.reload();
		onChanged();
	};
	let fields: RDSField[] = [];
	if (action === "Create") {
		const known =
			snapshot?.DBClusterIdentifier ?? snapshot?.DBInstanceIdentifier;
		const preselected = [scope.database, known].find(
			(id) => id && sources.data?.includes(id),
		);
		fields = [
			{
				key: cluster ? "DBClusterIdentifier" : "DBInstanceIdentifier",
				label: cluster ? "Source DB cluster" : "Source DB instance",
				options: sources.data ?? [],
				value: preselected,
				required: true,
			},
			{ key: field, label: "Snapshot identifier", required: true },
		];
	}
	if (action === "Copy")
		fields = [
			{
				key: targetField,
				label: "Target snapshot identifier",
				required: true,
			},
			{
				key: "CopyTags",
				label: "Copy source snapshot tags",
				type: "boolean",
				value: true,
			},
		];
	if (action === "Restore") {
		fields = [
			{
				key: restoreField,
				label: cluster ? "New cluster identifier" : "New instance identifier",
				required: true,
			},
			{ key: "DBSubnetGroupName", label: "Subnet group (optional)" },
			{
				key: cluster ? "DBClusterParameterGroupName" : "DBParameterGroupName",
				label: "Parameter group (optional)",
			},
		];
		if (!cluster)
			fields.push(
				{
					key: "DBInstanceClass",
					label: "Instance class (blank inherits source)",
				},
				{
					key: "AllocatedStorage",
					label: "Storage GiB (blank inherits source)",
					type: "number",
				},
			);
	}

	const operation =
		action === "Restore"
			? cluster
				? "RestoreDBClusterFromSnapshot"
				: "RestoreDBInstanceFromDBSnapshot"
			: `${action}${kind}`;
	const sourceInput: Record<string, unknown> = {};
	if (action === "Copy") sourceInput[`Source${field}`] = selected;
	else if (action === "Restore" && cluster) {
		sourceInput.SnapshotIdentifier = selected;
		sourceInput.Engine = snapshot?.Engine;
	} else if (action !== "Create") sourceInput[field] = selected;

	return (
		<SpaceBetween size="l">
			<Alert type="info">
				Snapshots capture metadata in records mode. Docker cluster snapshots and
				restores support Aurora and DocumentDB.
			</Alert>
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				items={poll.data ?? []}
				loading={poll.loading}
				selectionType="single"
				trackBy={identifier}
				selectedItems={snapshot ? [snapshot] : []}
				onSelectionChange={({ detail }) =>
					setSelected(
						detail.selectedItems[0] ? identifier(detail.selectedItems[0]) : "",
					)
				}
				empty={<Box>No snapshots in this account and Region.</Box>}
				header={
					<Header
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh snapshots"
									onClick={poll.reload}
								/>
								<ButtonDropdown
									onItemClick={({ detail }) => setAction(detail.id)}
									items={[
										{ id: "Copy", text: "Copy", disabled: !snapshotAvailable },
										{
											id: "Restore",
											text: cluster ? "Restore cluster" : "Restore instance",
											disabled: !snapshotAvailable,
										},
										{
											id: "Delete",
											text: "Delete",
											disabled: !snapshotAvailable,
										},
									]}
								>
									Actions
								</ButtonDropdown>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => setAction("Create")}
								>
									Create snapshot
								</Button>
							</SpaceBetween>
						}
					>
						{cluster ? "Cluster snapshots" : "Instance snapshots"}
					</Header>
				}
				columnDefinitions={[
					{ id: "id", header: "Snapshot identifier", cell: identifier },
					{
						id: "source",
						header: "Source",
						cell: (s) => s.DBInstanceIdentifier ?? s.DBClusterIdentifier ?? "—",
					},
					{
						id: "engine",
						header: "Engine",
						cell: (s) => `${s.Engine} ${s.EngineVersion}`,
					},
					{
						id: "status",
						header: "Status",
						cell: (s) => <DatabaseStatus status={s.Status} />,
					},
					{
						id: "created",
						header: "Created",
						cell: (s) => rdsDate(s.SnapshotCreateTime),
					},
				]}
			/>
			{snapshot && (
				<RDSTags
					{...{ scope, enabled }}
					arn={
						(cluster
							? snapshot.DBClusterSnapshotArn
							: snapshot.DBSnapshotArn) ?? ""
					}
					tags={snapshot.TagList}
					onChanged={changed}
				/>
			)}
			{snapshotAvailable && (
				<SnapshotRestoreAccess
					key={`${scope.account}/${scope.region}/${selected}`}
					{...{ scope, kind, field, selected }}
				/>
			)}
			{!!action && (
				<RDSOperationDialog
					key={`${action}/${selected}`}
					{...{ scope, fields, operation }}
					title={`${action} ${cluster ? "cluster snapshot" : "snapshot"}`}
					confirmation={action === "Delete" ? selected : undefined}
					input={sourceInput}
					validate={(input) => {
						if (action === "Create") validateName(String(input[field]), 255);
						if (action === "Copy")
							validateName(String(input[targetField]), 255);
						if (action === "Restore") validateName(String(input[restoreField]));
					}}
					onClose={() => setAction("")}
					onSaved={() => {
						setAction("");
						changed();
					}}
				/>
			)}
		</SpaceBetween>
	);
}

function SnapshotRestoreAccess({
	scope,
	kind,
	field,
	selected,
}: {
	scope: RDSScope;
	kind: string;
	field: string;
	selected: string;
}) {
	const input = { [field]: selected };
	const poll = usePoll(async () => {
		type Attribute = { AttributeName: string; AttributeValues?: string[] };
		const data = await rdsCall<Record<string, Record<string, Attribute[]>>>(
			scope,
			`Describe${kind}Attributes`,
			input,
		);
		const attrs = data[`${kind}AttributesResult`]?.[`${kind}Attributes`];
		return (
			attrs?.find((a) => a.AttributeName === "restore")?.AttributeValues ?? []
		);
	}, 0);
	const [editing, setEditing] = useState(false);
	return (
		<SpaceBetween size="xs">
			<Header
				variant="h3"
				actions={
					<Button onClick={() => setEditing(true)}>
						Modify restore access
					</Button>
				}
			>
				Snapshot restore access metadata
			</Header>
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{poll.data && <Box>{poll.data.join(", ") || "No shared accounts"}</Box>}
			{editing && (
				<RDSOperationDialog
					scope={scope}
					operation={`Modify${kind}Attribute`}
					title="Modify restore access metadata"
					input={{ ...input, AttributeName: "restore" }}
					fields={[
						{ key: "ValuesToAdd", label: "Account IDs or all to add" },
						{ key: "ValuesToRemove", label: "Account IDs or all to remove" },
					]}
					validate={(next) => {
						for (const key of ["ValuesToAdd", "ValuesToRemove"]) {
							if (next[key])
								next[key] = String(next[key])
									.split(/[\s,]+/)
									.filter(Boolean);
						}
					}}
					onClose={() => setEditing(false)}
					onSaved={() => {
						setEditing(false);
						poll.reload();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
