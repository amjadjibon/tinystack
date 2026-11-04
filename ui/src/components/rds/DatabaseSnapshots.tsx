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
		field = `${kind}Identifier`;
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
				key: "TargetDBSnapshotIdentifier",
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
	if (action === "Restore")
		fields = [
			{
				key: "DBInstanceIdentifier",
				label: "New instance identifier",
				required: true,
			},
			{
				key: "DBInstanceClass",
				label: "Instance class (blank inherits source)",
			},
			{
				key: "AllocatedStorage",
				label: "Storage GiB (blank inherits source)",
				type: "number",
			},
			{ key: "DBSubnetGroupName", label: "Subnet group (optional)" },
			{ key: "DBParameterGroupName", label: "Parameter group (optional)" },
		];
	const operation =
		action === "Restore"
			? "RestoreDBInstanceFromDBSnapshot"
			: `${action}${kind}`;
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				Snapshots preserve metadata in records mode. Cluster snapshots support
				Aurora only. Cluster restore is not implemented.
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
										...(!cluster
											? [
													{
														id: "Copy",
														text: "Copy",
														disabled:
															!enabled || snapshot?.Status !== "available",
													},
													{
														id: "Restore",
														text: "Restore instance",
														disabled:
															!enabled || snapshot?.Status !== "available",
													},
												]
											: []),
										{
											id: "Delete",
											text: "Delete",
											disabled: !enabled || snapshot?.Status !== "available",
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
			{!!action && (
				<RDSOperationDialog
					key={`${action}/${selected}`}
					{...{ scope, fields, operation }}
					title={`${action} ${cluster ? "cluster snapshot" : "snapshot"}`}
					confirmation={action === "Delete" ? selected : undefined}
					input={
						action === "Create"
							? {}
							: action === "Copy"
								? { SourceDBSnapshotIdentifier: selected }
								: { [field]: selected }
					}
					validate={(input) => {
						if (action === "Create") validateName(String(input[field]), 255);
						if (action === "Copy")
							validateName(String(input.TargetDBSnapshotIdentifier), 255);
						if (action === "Restore")
							validateName(String(input.DBInstanceIdentifier));
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
