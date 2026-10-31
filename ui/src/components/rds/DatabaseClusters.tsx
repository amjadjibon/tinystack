import {
	Alert,
	Box,
	Button,
	ButtonDropdown,
	Header,
	KeyValuePairs,
	Link,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import {
	clusterEngines,
	type DBCluster,
	finalSnapshotInput,
	type RDSScope,
	rdsHref,
	rdsPages,
	runtimeNote,
	validateName,
} from "#/lib/rds";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { DatabaseStatus } from "./DatabaseDetails";
import { type RDSField, RDSOperationDialog } from "./RDSOperationDialog";
import { RDSTags } from "./RDSTags";
export function DatabaseClusters({
	scope,
	enabled,
	onChanged,
}: {
	scope: RDSScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const poll = usePoll(() =>
		enabled
			? rdsPages<DBCluster>(scope, "DescribeDBClusters", "DBClusters")
			: Promise.resolve([]),
	);
	const [selected, setSelected] = useState<string>(""),
		[action, setAction] = useState("");
	const cluster = poll.data?.find((c) => c.DBClusterIdentifier === selected),
		follow = useFollow();
	const aurora = cluster?.Engine.startsWith("aurora"),
		available = cluster?.Status === "available";
	const changed = () => {
		poll.reload();
		onChanged();
	};
	let fields: RDSField[] = [];
	if (action === "Create")
		fields = [
			{
				key: "DBClusterIdentifier",
				label: "Cluster identifier",
				required: true,
			},
			{
				key: "Engine",
				label: "Engine",
				options: clusterEngines,
				value: "aurora-postgresql",
				required: true,
			},
			{
				key: "EngineVersion",
				label: "Engine version (blank uses engine default)",
			},
			{ key: "MasterUsername", label: "Master username (omit for Neptune)" },
			{
				key: "MasterUserPassword",
				label: "Master password (omit for Neptune)",
				type: "password",
			},
			{ key: "DBSubnetGroupName", label: "Subnet group (optional)" },
		];
	if (action === "Create" || action === "Modify")
		fields.push(
			{
				key: "BackupRetentionPeriod",
				label: "Backup retention days (1–35)",
				type: "number",
				value: String(
					cluster && action === "Modify"
						? (cluster.BackupRetentionPeriod ?? 1)
						: 1,
				),
			},
			{
				key: "DeletionProtection",
				label: "Deletion protection",
				type: "boolean",
				value: action === "Modify" && !!cluster?.DeletionProtection,
			},
		);
	if (action === "Modify" && cluster?.Engine !== "neptune")
		fields.push({
			key: "MasterUserPassword",
			label: "New master password (blank retains current)",
			type: "password",
		});
	if (action === "Delete")
		fields = [
			{
				key: "SkipFinalSnapshot",
				label: "Skip final snapshot",
				type: "boolean",
				value: true,
			},
			...(aurora
				? [
						{
							key: "FinalDBSnapshotIdentifier",
							label: "Final snapshot identifier (required unless skipping)",
						},
					]
				: []),
		];
	if (action === "Member")
		fields = [
			{
				key: "DBInstanceIdentifier",
				label: "Member instance identifier",
				required: true,
			},
			{
				key: "DBInstanceClass",
				label: "Instance class",
				value: "db.t3.medium",
				required: true,
			},
		];
	const operation =
		action === "Member"
			? "CreateDBInstance"
			: action.endsWith("HttpEndpoint")
				? action
				: `${action}DBCluster`;
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				{runtimeNote} DocumentDB and Neptune require Docker for stop/start and
				require skipping the final snapshot. Delete all members and disable
				deletion protection before deleting a cluster.
			</Alert>
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				items={poll.data ?? []}
				loading={poll.loading}
				selectionType="single"
				trackBy="DBClusterIdentifier"
				selectedItems={cluster ? [cluster] : []}
				onSelectionChange={({ detail }) =>
					setSelected(detail.selectedItems[0]?.DBClusterIdentifier ?? "")
				}
				empty={<Box>No clusters in this account and Region.</Box>}
				header={
					<Header
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh clusters"
									onClick={poll.reload}
								/>
								<ButtonDropdown
									onItemClick={({ detail }) => setAction(detail.id)}
									items={[
										{
											id: "Member",
											text: "Create member instance",
											disabled: !enabled || !available,
										},
										{
											id: "Modify",
											text: "Modify",
											disabled: !enabled || !available,
										},
										{
											id: "Stop",
											text: "Stop",
											disabled: !enabled || !available,
										},
										{
											id: "Start",
											text: "Start",
											disabled: !enabled || cluster?.Status !== "stopped",
										},
										{
											id: "EnableHttpEndpoint",
											text: "Enable Data API",
											disabled:
												!enabled ||
												!aurora ||
												!available ||
												!!cluster?.HttpEndpointEnabled,
										},
										{
											id: "DisableHttpEndpoint",
											text: "Disable Data API",
											disabled:
												!enabled ||
												!aurora ||
												!available ||
												!cluster?.HttpEndpointEnabled,
										},
										{
											id: "Delete",
											text: "Delete",
											disabled: !enabled || !cluster,
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
									Create cluster
								</Button>
							</SpaceBetween>
						}
					>
						Clusters
					</Header>
				}
				columnDefinitions={[
					{
						id: "id",
						header: "Identifier",
						cell: (c) => c.DBClusterIdentifier,
					},
					{
						id: "engine",
						header: "Engine",
						cell: (c) => `${c.Engine} ${c.EngineVersion}`,
					},
					{
						id: "status",
						header: "Status",
						cell: (c) => <DatabaseStatus status={c.Status} />,
					},
					{
						id: "http",
						header: "Data API",
						cell: (c) => (c.HttpEndpointEnabled ? "Enabled" : "Disabled"),
					},
					{
						id: "members",
						header: "Members",
						cell: (c) => (
							<SpaceBetween size="xs">
								{(c.DBClusterMembers ?? []).map((m) => (
									<Link
										key={m.DBInstanceIdentifier}
										onFollow={follow}
										href={rdsHref({
											...scope,
											section: "databases",
											database: m.DBInstanceIdentifier,
										})}
									>
										{m.DBInstanceIdentifier}
										{m.IsClusterWriter ? " (writer)" : " (reader)"}
									</Link>
								))}
							</SpaceBetween>
						),
					},
				]}
			/>
			{cluster && (
				<>
					<KeyValuePairs
						items={[
							{ label: "ARN", value: cluster.DBClusterArn },
							{ label: "Endpoint", value: cluster.Endpoint || "Pending" },
							{
								label: "Reader endpoint",
								value: cluster.ReaderEndpoint || "Pending",
							},
							{ label: "Port", value: cluster.Port || "Pending" },
							{
								label: "Deletion protection",
								value: cluster.DeletionProtection ? "Enabled" : "Disabled",
							},
						]}
					/>
					<RDSTags
						{...{ scope, enabled }}
						arn={cluster.DBClusterArn}
						tags={cluster.TagList}
						onChanged={changed}
					/>
				</>
			)}
			{action && (
				<RDSOperationDialog
					key={`${action}/${selected}`}
					{...{ scope, fields, operation }}
					title={
						action === "Member"
							? "Create member instance"
							: `${action.replace("HttpEndpoint", " Data API")} cluster`
					}
					confirmation={action === "Delete" ? selected : undefined}
					input={
						action === "Member"
							? { DBClusterIdentifier: selected, Engine: cluster?.Engine }
							: action === "Create"
								? {}
								: action.endsWith("HttpEndpoint")
									? { ResourceArn: cluster?.DBClusterArn }
									: {
											DBClusterIdentifier: selected,
											...(action === "Modify"
												? { ApplyImmediately: true }
												: {}),
										}
					}
					validate={(input) => {
						if (action === "Member")
							validateName(String(input.DBInstanceIdentifier));
						if (action === "Create")
							validateName(String(input.DBClusterIdentifier));
						if (action === "Delete") {
							if (input.SkipFinalSnapshot && input.FinalDBSnapshotIdentifier)
								throw new Error("Omit the final identifier when skipping.");
							Object.assign(
								input,
								finalSnapshotInput(
									input.SkipFinalSnapshot === true,
									String(input.FinalDBSnapshotIdentifier ?? ""),
								),
							);
						}
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
