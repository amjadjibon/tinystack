import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	Link,
	RadioGroup,
	Select,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { integer } from "#/lib/consoleActions";
import {
	type DynamoScope,
	type DynamoTable,
	streamViews,
	tableArn,
	tableCall,
} from "#/lib/dynamodb";
import { type LambdaMapping, lambdaCall, lambdaHref } from "#/lib/lambda";
import { useFollow } from "#/lib/useFollow";

export function DynamoStreams({
	scope,
	table,
	resources,
	enabled,
	lambdaEnabled,
	onChanged,
}: {
	scope: DynamoScope;
	table: DynamoTable;
	resources: Resource[];
	enabled: boolean;
	lambdaEnabled: boolean;
	onChanged: (message: string) => void;
}) {
	const follow = useFollow();
	const streaming = table.StreamSpecification?.StreamEnabled === true;
	const streamArn = streaming ? table.LatestStreamArn : undefined;
	const mappings = resources
		.filter(
			(r) =>
				r.service === "lambda" &&
				r.type === "event-source-mapping" &&
				r.account === scope.account &&
				r.region === scope.region,
		)
		.map((r) => r.details as LambdaMapping)
		.filter((m) => m.EventSourceArn.startsWith(`${tableArn(scope)}/stream/`));
	const functions = resources.filter(
		(r) =>
			r.service === "lambda" &&
			r.type === "function" &&
			r.account === scope.account &&
			r.region === scope.region,
	);
	const [dialog, setDialog] = useState<
		"stream" | "connect" | "toggle" | "delete" | null
	>(null);
	const [selected, setSelected] = useState<LambdaMapping>();
	const [streamEnabled, setStreamEnabled] = useState(false),
		[view, setView] = useState("NEW_AND_OLD_IMAGES");
	const [functionName, setFunctionName] = useState(""),
		[position, setPosition] = useState("LATEST"),
		[batch, setBatch] = useState("100");
	const [active, setActive] = useState(true),
		[partial, setPartial] = useState(false),
		[retries, setRetries] = useState("-1");
	return (
		<SpaceBetween size="l">
			<Container
				header={
					<Header
						variant="h2"
						actions={
							<Button
								disabled={!enabled || !table.TableName}
								onClick={() => {
									setStreamEnabled(streaming);
									setView(
										table.StreamSpecification?.StreamViewType ??
											"NEW_AND_OLD_IMAGES",
									);
									setDialog("stream");
								}}
							>
								Edit
							</Button>
						}
					>
						DynamoDB Streams
					</Header>
				}
			>
				<SpaceBetween size="l">
					<KeyValuePairs
						columns={2}
						items={[
							{ label: "Status", value: streaming ? "Enabled" : "Disabled" },
							{
								label: "View type",
								value: table.StreamSpecification?.StreamViewType ?? "—",
							},
							{ label: "Current stream ARN", value: streamArn ?? "—" },
						]}
					/>
					<Alert type="info">
						Streams record item changes for 24 hours. Changing the view type
						disables the current stream and creates a new stream ARN. Existing
						Lambda mappings keep their original source; reconnect them to the
						new stream.
					</Alert>
				</SpaceBetween>
			</Container>
			<Table
				items={mappings}
				trackBy="UUID"
				wrapLines
				header={
					<Header
						variant="h2"
						counter={`(${mappings.length})`}
						description="Mappings for current and previous streams of this table."
						actions={
							<Button
								variant="primary"
								disabled={!lambdaEnabled || !streamArn || !functions.length}
								onClick={() => {
									setFunctionName(functions[0].arn);
									setPosition("LATEST");
									setBatch("100");
									setActive(true);
									setPartial(false);
									setRetries("-1");
									setDialog("connect");
								}}
							>
								Connect Lambda function
							</Button>
						}
					>
						Lambda connections
					</Header>
				}
				columnDefinitions={[
					{
						id: "function",
						header: "Function",
						cell: (m) => (
							<Link
								href={lambdaHref({
									account: scope.account,
									region: scope.region,
									functionName: m.FunctionArn.split(":")[6],
								})}
								onFollow={follow}
							>
								{m.FunctionArn}
							</Link>
						),
					},
					{ id: "stream", header: "Stream ARN", cell: (m) => m.EventSourceArn },
					{ id: "state", header: "State", cell: (m) => m.State },
					{
						id: "result",
						header: "Last processing result",
						cell: (m) => m.LastProcessingResult ?? "—",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (m) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!lambdaEnabled}
									onClick={() => {
										setSelected(m);
										setActive(m.State !== "Enabled");
										setDialog("toggle");
									}}
								>
									{m.State === "Enabled" ? "Disable" : "Enable"}
								</Button>
								<Button
									disabled={!lambdaEnabled}
									onClick={() => {
										setSelected(m);
										setDialog("delete");
									}}
								>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						No Lambda connections. Enable streams and create a Lambda function
						in this account and Region to connect them.
					</Box>
				}
			/>
			{dialog && (
				<ActionDialog
					title={
						dialog === "stream"
							? "Edit DynamoDB Streams"
							: dialog === "connect"
								? "Connect Lambda function"
								: dialog === "delete"
									? "Delete Lambda connection"
									: `${active ? "Enable" : "Disable"} Lambda connection`
					}
					submit={
						dialog === "connect"
							? "Connect function"
							: dialog === "delete"
								? "Delete connection"
								: "Save changes"
					}
					confirmation={dialog === "delete" ? "delete" : undefined}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						onChanged(
							dialog === "stream"
								? "DynamoDB Streams updated."
								: dialog === "delete"
									? "Lambda connection deleted."
									: "Lambda connection updated.",
						);
					}}
					plan={async () => {
						if (dialog === "stream") {
							if (
								streamEnabled === streaming &&
								(!streamEnabled ||
									view === table.StreamSpecification?.StreamViewType)
							)
								return [];
							return [
								...(streaming
									? [
											{
												label: "Disable current stream",
												run: () =>
													tableCall(scope, "UpdateTable", {
														StreamSpecification: { StreamEnabled: false },
													}),
											},
										]
									: []),
								...(streamEnabled
									? [
											{
												label: "Enable stream",
												run: () =>
													tableCall(scope, "UpdateTable", {
														StreamSpecification: {
															StreamEnabled: true,
															StreamViewType: view,
														},
													}),
											},
										]
									: []),
							];
						}
						if (dialog === "delete")
							return [
								{
									label: "Delete mapping",
									run: () =>
										lambdaCall(scope, "DeleteEventSourceMapping", {
											UUID: selected?.UUID,
										}),
								},
							];
						if (dialog === "toggle")
							return [
								{
									label: "Update mapping",
									run: () =>
										lambdaCall(scope, "UpdateEventSourceMapping", {
											UUID: selected?.UUID,
											Enabled: active,
										}),
								},
							];
						if (!streamArn || !functions.some((f) => f.arn === functionName))
							throw new Error(
								"Choose an existing function and an enabled stream in this account and Region.",
							);
						const input = {
							FunctionName: functionName,
							EventSourceArn: streamArn,
							StartingPosition: position,
							Enabled: active,
							BatchSize: integer(batch, "Batch size", 1, 10000),
							MaximumRetryAttempts:
								retries === "-1"
									? -1
									: integer(retries, "Retry attempts", 0, 10000),
							FunctionResponseTypes: partial ? ["ReportBatchItemFailures"] : [],
						};
						return [
							{
								label: "Connect Lambda function",
								run: () => lambdaCall(scope, "CreateEventSourceMapping", input),
							},
						];
					}}
				>
					{dialog === "stream" ? (
						<SpaceBetween size="l">
							<Checkbox
								checked={streamEnabled}
								onChange={({ detail }) => setStreamEnabled(detail.checked)}
							>
								Enable streams
							</Checkbox>
							{streamEnabled && (
								<FormField label="Stream view type">
									<Select
										selectedOption={{ value: view, label: view }}
										options={streamViews.map((v) => ({ value: v, label: v }))}
										onChange={({ detail }) =>
											setView(
												detail.selectedOption.value ?? "NEW_AND_OLD_IMAGES",
											)
										}
									/>
								</FormField>
							)}
						</SpaceBetween>
					) : dialog === "connect" ? (
						<SpaceBetween size="l">
							<FormField label="Lambda function">
								<Select
									selectedOption={{
										value: functionName,
										label:
											functions.find((f) => f.arn === functionName)?.name ??
											functionName,
									}}
									options={functions.map((f) => ({
										value: f.arn,
										label: f.name,
									}))}
									onChange={({ detail }) =>
										setFunctionName(detail.selectedOption.value ?? "")
									}
								/>
							</FormField>
							<FormField label="Starting position">
								<RadioGroup
									value={position}
									onChange={({ detail }) => setPosition(detail.value)}
									items={[
										{
											value: "LATEST",
											label: "Latest",
											description:
												"Process changes written after this connection is created.",
										},
										{
											value: "TRIM_HORIZON",
											label: "Trim horizon",
											description:
												"Start with the oldest records still retained in the stream.",
										},
									]}
								/>
							</FormField>
							<FormField label="Batch size" constraintText="1–10,000 records">
								<Input
									ariaLabel="Stream batch size"
									type="number"
									value={batch}
									onChange={({ detail }) => setBatch(detail.value)}
								/>
							</FormField>
							<FormField
								label="Maximum retry attempts"
								constraintText="0–10,000, or -1 for unlimited"
							>
								<Input
									ariaLabel="Stream maximum retries"
									type="number"
									value={retries}
									onChange={({ detail }) => setRetries(detail.value)}
								/>
							</FormField>
							<Checkbox
								checked={partial}
								onChange={({ detail }) => setPartial(detail.checked)}
							>
								Report individual batch item failures
							</Checkbox>
							<Checkbox
								checked={active}
								onChange={({ detail }) => setActive(detail.checked)}
							>
								Enable connection
							</Checkbox>
							<Alert type="info">
								An enabled connection reads stream records and executes your
								function. Docker is required for execution. Edit additional
								mapping settings from the function's Triggers tab.
							</Alert>
						</SpaceBetween>
					) : (
						<Alert type={dialog === "delete" ? "warning" : "info"}>
							{dialog === "delete"
								? "Delete this event source mapping. The table, stream and function remain."
								: active
									? "Enable processing of stream records by this function."
									: "Stop processing further stream records with this mapping."}{" "}
							{selected?.FunctionArn}
						</Alert>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
