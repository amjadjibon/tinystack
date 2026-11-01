import {
	Alert,
	Button,
	Container,
	Header,
	KeyValuePairs,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { errorText } from "#/lib/consoleActions";
import {
	type ContinuousBackups,
	type DynamoScope,
	dynamoCall,
	listBackups,
	tableCall,
} from "#/lib/dynamodb";
import { dynamoName } from "#/lib/dynamodbHelpers";
import { usePoll } from "#/lib/usePoll";
import { DynamoAction } from "./DynamoAction";

export function DynamoBackups({
	scope,
	enabled,
	onChanged,
}: {
	scope: DynamoScope;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const backups = usePoll(async () => (enabled ? listBackups(scope) : []), 0);
	const pitr = usePoll<ContinuousBackups>(
		async () =>
			enabled
				? tableCall<ContinuousBackups>(scope, "DescribeContinuousBackups")
				: {},
		0,
	);
	const [detail, setDetail] = useState<unknown>();
	const [error, setError] = useState("");
	const recovery =
		pitr.data?.ContinuousBackupsDescription?.PointInTimeRecoveryDescription;
	const active = recovery?.PointInTimeRecoveryStatus === "ENABLED";
	const saved = (message: string) => () => {
		backups.reload();
		pitr.reload();
		onChanged(message);
	};
	return (
		<SpaceBetween size="l">
			{(backups.error || pitr.error || error) && (
				<Alert type="error">{backups.error || pitr.error || error}</Alert>
			)}
			<Container
				header={
					<Header
						variant="h2"
						actions={
							<Button
								onClick={() => {
									backups.reload();
									pitr.reload();
								}}
							>
								Refresh
							</Button>
						}
					>
						Point-in-time recovery
					</Header>
				}
			>
				<SpaceBetween size="m">
					<KeyValuePairs
						items={[
							{
								label: "Status",
								value: recovery?.PointInTimeRecoveryStatus ?? "DISABLED",
							},
							...(
								[
									"EarliestRestorableDateTime",
									"LatestRestorableDateTime",
								] as const
							).map((k) => ({
								label:
									k === "EarliestRestorableDateTime"
										? "Earliest restorable time"
										: "Latest restorable time",
								value: recovery?.[k]
									? new Date(recovery[k] * 1000).toLocaleString()
									: "—",
							})),
						]}
					/>
					<Alert>
						TinyStack restores the latest state only. Historical snapshots are
						not retained.
					</Alert>
					<SpaceBetween direction="horizontal" size="xs">
						<DynamoAction
							label={active ? "Disable recovery" : "Enable recovery"}
							enabled={enabled && !pitr.loading && !pitr.error}
							run={() =>
								tableCall(scope, "UpdateContinuousBackups", {
									PointInTimeRecoverySpecification: {
										PointInTimeRecoveryEnabled: !active,
									},
								})
							}
							onSaved={saved(
								active
									? "Point-in-time recovery disabled."
									: "Point-in-time recovery enabled.",
							)}
						/>
						<DynamoAction
							label="Restore latest"
							enabled={enabled && active}
							fields={[
								{
									key: "target",
									label: "Target table name",
									validate: dynamoName,
								},
							]}
							run={(v) =>
								dynamoCall(scope, "RestoreTableToPointInTime", {
									SourceTableName: scope.table,
									TargetTableName: dynamoName(v.target),
									UseLatestRestorableTime: true,
								})
							}
							onSaved={saved("Table restore started.")}
						/>
					</SpaceBetween>
				</SpaceBetween>
			</Container>
			<Table
				items={backups.data ?? []}
				loading={backups.loading}
				header={
					<Header
						variant="h2"
						actions={
							<DynamoAction
								label="Create backup"
								enabled={enabled}
								fields={[
									{ key: "name", label: "Backup name", validate: dynamoName },
								]}
								run={(v) =>
									tableCall(scope, "CreateBackup", {
										BackupName: dynamoName(v.name),
									})
								}
								onSaved={saved("Backup created.")}
							/>
						}
					>
						Backups
					</Header>
				}
				columnDefinitions={[
					{ id: "name", header: "Name", cell: (b) => b.BackupName },
					{ id: "status", header: "Status", cell: (b) => b.BackupStatus },
					{
						id: "actions",
						header: "Actions",
						cell: (b) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!enabled}
									onClick={async () => {
										try {
											setDetail(
												await dynamoCall(scope, "DescribeBackup", {
													BackupArn: b.BackupArn,
												}),
											);
											setError("");
										} catch (e) {
											setError(errorText(e));
										}
									}}
								>
									Details
								</Button>
								<DynamoAction
									label="Restore backup"
									enabled={enabled}
									fields={[
										{
											key: "target",
											label: "Target table name",
											validate: dynamoName,
										},
									]}
									run={(v) =>
										dynamoCall(scope, "RestoreTableFromBackup", {
											BackupArn: b.BackupArn,
											TargetTableName: dynamoName(v.target),
										})
									}
									onSaved={saved("Table restore started.")}
								/>
								<DynamoAction
									label="Delete backup"
									enabled={enabled}
									destructive
									run={() =>
										dynamoCall(scope, "DeleteBackup", {
											BackupArn: b.BackupArn,
										})
									}
									onSaved={saved("Backup deleted.")}
								/>
							</SpaceBetween>
						),
					},
				]}
				empty="No backups."
			/>
			{detail !== undefined && (
				<Container header={<Header variant="h2">Backup details</Header>}>
					<CodeBlock text={JSON.stringify(detail, null, 2)} />
				</Container>
			)}
		</SpaceBetween>
	);
}
