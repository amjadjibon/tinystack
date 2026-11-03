import {
	Alert,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	RadioGroup,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import type { Resource } from "#/lib/api";
import { type ActionStep, stringMap } from "#/lib/consoleActions";
import {
	capacity,
	type DynamoScope,
	type DynamoTable,
	dynamoCall,
	tableArn,
	tableCall,
} from "#/lib/dynamodb";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";

export function DynamoSettings({
	scope,
	table,
	resource,
	enabled,
	onChanged,
}: {
	scope: DynamoScope;
	table: DynamoTable;
	resource?: Resource;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const ttl = usePoll(async () =>
		enabled
			? (
					await tableCall<{
						TimeToLiveDescription?: {
							AttributeName?: string;
							TimeToLiveStatus?: string;
						};
					}>(scope, "DescribeTimeToLive")
				).TimeToLiveDescription
			: undefined,
	);
	const tags = usePoll(async () =>
		enabled
			? Object.fromEntries(
					(
						(
							await dynamoCall<{ Tags?: { Key: string; Value: string }[] }>(
								scope,
								"ListTagsOfResource",
								{ ResourceArn: tableArn(scope) },
							)
						).Tags ?? []
					).map((t) => [t.Key, t.Value]),
				)
			: (resource?.tags ?? {}),
	);
	const [dialog, setDialog] = useState<"capacity" | "ttl" | "tags" | null>(
		null,
	);
	const [mode, setMode] = useState("PAY_PER_REQUEST"),
		[read, setRead] = useState("5"),
		[write, setWrite] = useState("5");
	const [indexCapacity, setIndexCapacity] = useState<
		Record<string, { read: string; write: string }>
	>({});
	const [expire, setExpire] = useState(false),
		[attribute, setAttribute] = useState("");
	const [text, setText] = useState("{}");
	const open = (mode: "capacity" | "ttl" | "tags") => {
		setDialog(mode);
		setMode(table.BillingModeSummary?.BillingMode ?? "PROVISIONED");
		setRead(String(table.ProvisionedThroughput?.ReadCapacityUnits || 5));
		setWrite(String(table.ProvisionedThroughput?.WriteCapacityUnits || 5));
		setIndexCapacity(
			Object.fromEntries(
				(table.GlobalSecondaryIndexes ?? []).map((i) => [
					i.IndexName,
					{
						read: String(i.ProvisionedThroughput?.ReadCapacityUnits || 5),
						write: String(i.ProvisionedThroughput?.WriteCapacityUnits || 5),
					},
				]),
			),
		);
		setExpire(ttl.data?.TimeToLiveStatus === "ENABLED");
		setAttribute(ttl.data?.AttributeName ?? "expiresAt");
		setText(JSON.stringify(tags.data ?? {}, null, 2));
	};
	const saved = () => {
		setDialog(null);
		ttl.reload();
		tags.reload();
		onChanged(
			dialog === "ttl"
				? "Time to live updated."
				: dialog === "tags"
					? "Table tags updated."
					: "Read/write capacity updated.",
		);
	};
	const plan = async (): Promise<ActionStep[]> => {
		if (dialog === "capacity") {
			const input = {
				BillingMode: mode,
				...(mode === "PROVISIONED"
					? {
							ProvisionedThroughput: capacity(read, write),
							GlobalSecondaryIndexUpdates: Object.entries(indexCapacity).map(
								([IndexName, c]) => ({
									Update: {
										IndexName,
										ProvisionedThroughput: capacity(c.read, c.write),
									},
								}),
							),
						}
					: {}),
			};
			return [
				{
					label: "Update capacity",
					run: () => tableCall(scope, "UpdateTable", input),
				},
			];
		}
		if (dialog === "ttl") {
			if (
				expire &&
				(!attribute || new TextEncoder().encode(attribute).length > 255)
			)
				throw new Error("TTL attribute name must use 1–255 bytes.");
			const current = ttl.data?.TimeToLiveStatus === "ENABLED";
			if (
				expire === current &&
				(!expire || attribute === ttl.data?.AttributeName)
			)
				return [];
			return [
				...(current
					? [
							{
								label: "Disable current TTL",
								run: () =>
									tableCall(scope, "UpdateTimeToLive", {
										TimeToLiveSpecification: {
											Enabled: false,
											AttributeName: ttl.data?.AttributeName,
										},
									}),
							},
						]
					: []),
				...(expire
					? [
							{
								label: "Enable TTL",
								run: () =>
									tableCall(scope, "UpdateTimeToLive", {
										TimeToLiveSpecification: {
											Enabled: true,
											AttributeName: attribute,
										},
									}),
							},
						]
					: []),
			];
		}
		const next = stringMap(text, "Tags"),
			removed = Object.keys(tags.data ?? {}).filter((k) => !(k in next));
		return [
			...(removed.length
				? [
						{
							label: "Remove tags",
							run: () =>
								dynamoCall(scope, "UntagResource", {
									ResourceArn: tableArn(scope),
									TagKeys: removed,
								}),
						},
					]
				: []),
			...(Object.keys(next).length
				? [
						{
							label: "Save tags",
							run: () =>
								dynamoCall(scope, "TagResource", {
									ResourceArn: tableArn(scope),
									Tags: tagList(next),
								}),
						},
					]
				: []),
		];
	};
	return (
		<SpaceBetween size="l">
			{(ttl.error || tags.error) && (
				<Alert type="error">{ttl.error || tags.error}</Alert>
			)}
			<Container
				header={
					<Header
						variant="h2"
						description="Capacity settings are stored without throughput enforcement or charges."
						actions={
							<Button
								disabled={!enabled || !table.TableName}
								onClick={() => open("capacity")}
							>
								Edit
							</Button>
						}
					>
						Read/write capacity
					</Header>
				}
			>
				<KeyValuePairs
					columns={3}
					items={[
						{
							label: "Capacity mode",
							value: table.BillingModeSummary?.BillingMode ?? "PROVISIONED",
						},
						{
							label: "Read units",
							value: table.ProvisionedThroughput?.ReadCapacityUnits ?? 0,
						},
						{
							label: "Write units",
							value: table.ProvisionedThroughput?.WriteCapacityUnits ?? 0,
						},
					]}
				/>
			</Container>
			<Container
				header={
					<Header
						variant="h2"
						description="Expired items are removed by TinyStack's background sweeper. Store epoch seconds in a Number attribute."
						actions={
							<Button
								disabled={!enabled || !ttl.data}
								onClick={() => open("ttl")}
							>
								Edit TTL
							</Button>
						}
					>
						Time to live (TTL)
					</Header>
				}
			>
				<KeyValuePairs
					columns={2}
					items={[
						{
							label: "Status",
							value:
								ttl.data?.TimeToLiveStatus ??
								"Read through a signed API request",
						},
						{ label: "Attribute", value: ttl.data?.AttributeName ?? "—" },
					]}
				/>
			</Container>
			<Container
				header={
					<Header
						variant="h2"
						counter={`(${Object.keys(tags.data ?? {}).length})`}
						actions={
							<Button
								disabled={!enabled || !tags.data}
								onClick={() => open("tags")}
							>
								Edit tags
							</Button>
						}
					>
						Tags
					</Header>
				}
			>
				<CodeBlock
					text={JSON.stringify(tags.data ?? resource?.tags ?? {}, null, 2)}
				/>
			</Container>
			{dialog && (
				<ActionDialog
					title={
						dialog === "ttl"
							? "Edit time to live"
							: dialog === "tags"
								? "Edit tags"
								: "Edit read/write capacity"
					}
					onClose={() => setDialog(null)}
					onSaved={saved}
					plan={plan}
				>
					{dialog === "ttl" ? (
						<SpaceBetween size="l">
							<Checkbox
								checked={expire}
								onChange={({ detail }) => setExpire(detail.checked)}
							>
								Enable time to live
							</Checkbox>
							<FormField label="TTL attribute name">
								<Input
									ariaLabel="TTL attribute name"
									value={attribute}
									disabled={!expire}
									onChange={({ detail }) => setAttribute(detail.value)}
								/>
							</FormField>
							<Alert type="info">
								Enabling TTL can delete expired items. Changing its attribute
								disables the current setting before enabling the new one.
							</Alert>
						</SpaceBetween>
					) : dialog === "tags" ? (
						<FormField
							label="Tags (JSON)"
							description="Object of string values; use {} to remove all tags."
						>
							<Textarea
								ariaLabel="Table tags"
								rows={8}
								value={text}
								onChange={({ detail }) => setText(detail.value)}
							/>
						</FormField>
					) : (
						<SpaceBetween size="l">
							<RadioGroup
								value={mode}
								onChange={({ detail }) => setMode(detail.value)}
								items={[
									{ value: "PAY_PER_REQUEST", label: "On-demand" },
									{ value: "PROVISIONED", label: "Provisioned" },
								]}
							/>
							{mode === "PROVISIONED" && (
								<SpaceBetween size="l">
									<SpaceBetween direction="horizontal" size="s">
										<FormField label="Read capacity units">
											<Input
												ariaLabel="Table read capacity units"
												type="number"
												value={read}
												onChange={({ detail }) => setRead(detail.value)}
											/>
										</FormField>
										<FormField label="Write capacity units">
											<Input
												ariaLabel="Table write capacity units"
												type="number"
												value={write}
												onChange={({ detail }) => setWrite(detail.value)}
											/>
										</FormField>
									</SpaceBetween>
									{Object.entries(indexCapacity).map(([name, c]) => (
										<Container
											key={name}
											header={<Header variant="h3">{name}</Header>}
										>
											<SpaceBetween direction="horizontal" size="s">
												<FormField label="Index read capacity units">
													<Input
														ariaLabel={`${name} read capacity units`}
														type="number"
														value={c.read}
														onChange={({ detail }) =>
															setIndexCapacity({
																...indexCapacity,
																[name]: { ...c, read: detail.value },
															})
														}
													/>
												</FormField>
												<FormField label="Index write capacity units">
													<Input
														ariaLabel={`${name} write capacity units`}
														type="number"
														value={c.write}
														onChange={({ detail }) =>
															setIndexCapacity({
																...indexCapacity,
																[name]: { ...c, write: detail.value },
															})
														}
													/>
												</FormField>
											</SpaceBetween>
										</Container>
									))}
								</SpaceBetween>
							)}
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
