import {
	Alert,
	Button,
	Container,
	ContentLayout,
	CopyToClipboard,
	Header,
	KeyValuePairs,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import {
	type DynamoScope,
	type DynamoTable as TableDetails,
	tableArn,
	tableCall,
	tableDetails,
} from "#/lib/dynamodb";
import { usePoll } from "#/lib/usePoll";
import { DynamoBackups } from "./DynamoBackups";
import { DynamoExports } from "./DynamoExports";
import { DynamoIndexes } from "./DynamoIndexes";
import { DynamoItems } from "./DynamoItems";
import { DynamoKinesis } from "./DynamoKinesis";
import { DynamoPartiQL } from "./DynamoPartiQL";
import { DynamoSettings } from "./DynamoSettings";
import { DynamoStreams } from "./DynamoStreams";

export function DynamoTable({
	scope,
	resource,
	resources,
	enabled,
	lambdaEnabled,
	loading,
	initialTab,
	onRefresh,
	onChanged,
	onDeleted,
}: {
	scope: DynamoScope;
	resource?: Resource;
	resources: Resource[];
	enabled: boolean;
	lambdaEnabled: boolean;
	loading: boolean;
	initialTab: string;
	onRefresh: () => void;
	onChanged: (message: string) => void;
	onDeleted: () => void;
}) {
	const details = usePoll<TableDetails>(async () =>
		enabled
			? ((await tableCall<{ Table?: TableDetails }>(scope, "DescribeTable"))
					.Table ?? {})
			: tableDetails(resource),
	);
	const table = details.data ?? tableDetails(resource);
	const [tab, setTab] = useState(initialTab),
		[deleting, setDeleting] = useState(false);
	const refresh = (message?: string) => {
		details.reload();
		onRefresh();
		if (message) onChanged(message);
	};
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh table"
								onClick={() => refresh()}
							/>
							<Button
								disabled={!table.TableName}
								onClick={() => setTab("items")}
							>
								Explore table items
							</Button>
							<Button
								disabled={!enabled || !table.TableName}
								onClick={() => setDeleting(true)}
							>
								Delete
							</Button>
						</SpaceBetween>
					}
				>
					{scope.table}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{details.error && (
					<Alert type="error" header="Cannot load table">
						{details.error}
					</Alert>
				)}
				{!loading && !details.loading && !table.TableName && !resource && (
					<Alert type="warning">
						This table was not found in the selected account and Region.
					</Alert>
				)}
				<Container header={<Header variant="h2">Table overview</Header>}>
					<KeyValuePairs
						columns={3}
						items={[
							{
								label: "Partition key",
								value:
									table.KeySchema?.find((k) => k.KeyType === "HASH")
										?.AttributeName ?? "—",
							},
							{
								label: "Sort key",
								value:
									table.KeySchema?.find((k) => k.KeyType === "RANGE")
										?.AttributeName ?? "—",
							},
							{ label: "Status", value: table.TableStatus ?? "—" },
							{ label: "Items", value: table.ItemCount ?? 0 },
							{
								label: "Table size",
								value: `${table.TableSizeBytes ?? 0} bytes`,
							},
							{
								label: "Capacity mode",
								value: table.BillingModeSummary?.BillingMode ?? "PROVISIONED",
							},
							{
								label: "Table ARN",
								value: (
									<CopyToClipboard
										variant="inline"
										textToCopy={table.TableArn ?? tableArn(scope)}
										copySuccessText="ARN copied"
										copyErrorText="ARN failed to copy"
									/>
								),
							},
							{
								label: "Created",
								value: table.CreationDateTime
									? new Date(table.CreationDateTime * 1000).toLocaleString()
									: "—",
							},
						]}
					/>
				</Container>
				<Tabs
					activeTabId={tab}
					onChange={({ detail }) => setTab(detail.activeTabId)}
					tabs={[
						{
							id: "items",
							label: "Explore items",
							content: (
								<DynamoItems
									scope={scope}
									table={table}
									enabled={enabled && !!table.TableName}
									onChanged={(message) => refresh(message)}
								/>
							),
						},
						{
							id: "indexes",
							label: "Indexes",
							content: (
								<DynamoIndexes
									scope={scope}
									table={table}
									enabled={enabled}
									onChanged={(message) => refresh(message)}
								/>
							),
						},
						{
							id: "settings",
							label: "Additional settings",
							content: (
								<DynamoSettings
									scope={scope}
									table={table}
									resource={resource}
									enabled={enabled}
									onChanged={(message) => refresh(message)}
								/>
							),
						},
						{
							id: "streams",
							label: "Streams and Lambda",
							content: (
								<DynamoStreams
									scope={scope}
									table={table}
									resources={resources}
									enabled={enabled}
									lambdaEnabled={lambdaEnabled && !!table.TableName}
									onChanged={(message) => refresh(message)}
								/>
							),
						},
						{
							id: "backups",
							label: "Backups",
							content: (
								<DynamoBackups
									key={String(enabled && !!table.TableName)}
									scope={scope}
									enabled={enabled && !!table.TableName}
									onChanged={refresh}
								/>
							),
						},
						{
							id: "exports",
							label: "Exports & streaming",
							content: (
								<SpaceBetween size="l">
									<DynamoExports
										key={String(enabled && !!table.TableName)}
										scope={scope}
										enabled={enabled && !!table.TableName}
									/>
									<DynamoKinesis
										key={String(enabled && !!table.TableName)}
										scope={scope}
										enabled={enabled && !!table.TableName}
									/>
								</SpaceBetween>
							),
						},
						{
							id: "partiql",
							label: "PartiQL",
							content: (
								<DynamoPartiQL
									scope={scope}
									enabled={enabled && !!table.TableName}
									onChanged={refresh}
								/>
							),
						},
					]}
				/>
				{deleting && (
					<ActionDialog
						title="Delete table"
						submit="Delete table"
						confirmation={scope.table}
						onClose={() => setDeleting(false)}
						onSaved={() => {
							setDeleting(false);
							onDeleted();
						}}
						plan={async () => [
							{
								label: "Delete table",
								run: () => tableCall(scope, "DeleteTable"),
							},
						]}
					>
						<Alert type="warning">
							Delete {scope.table} and all its items and indexes. This cannot be
							undone. Lambda mappings remain; delete or disable them from
							Streams and Lambda before deleting the table.
						</Alert>
					</ActionDialog>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}
