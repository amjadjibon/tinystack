import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	CopyToClipboard,
	FormField,
	Header,
	KeyValuePairs,
	Link,
	SpaceBetween,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import type { Resource } from "#/lib/api";
import { type ActionStep, stringMap } from "#/lib/consoleActions";
import { iamHref } from "#/lib/iam";
import {
	codeDefaults,
	configurationInput,
	deploymentCode,
	type LambdaFunction as FunctionDetails,
	functionArn,
	functionCall,
	functionConfig,
	functionMappings,
	type LambdaScope,
	lambdaCall,
	settingsDefaults,
} from "#/lib/lambda";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { LambdaCodeInput } from "./LambdaCodeInput";
import { LambdaLogs } from "./LambdaLogs";
import { LambdaSettings } from "./LambdaSettings";
import { LambdaTest } from "./LambdaTest";
import { LambdaTriggers } from "./LambdaTriggers";
import { LambdaUrls } from "./LambdaUrls";

export function LambdaFunction({
	scope,
	resource,
	resources,
	enabled,
	snsEnabled,
	logsEnabled,
	loading,
	onRefresh,
	onChanged,
	onDeleted,
}: {
	scope: LambdaScope;
	resource?: Resource;
	resources: Resource[];
	enabled: boolean;
	snsEnabled: boolean;
	logsEnabled: boolean;
	loading: boolean;
	onRefresh: () => void;
	onChanged: (message: string) => void;
	onDeleted: () => void;
}) {
	const follow = useFollow();
	const details = usePoll<FunctionDetails>(async () =>
		enabled
			? functionCall<FunctionDetails>(scope, "GetFunction")
			: { Configuration: functionConfig(resource), Tags: resource?.tags },
	);
	const config = details.data?.Configuration ?? functionConfig(resource);
	const tags = details.data?.Tags ?? resource?.tags ?? {};
	const arn = functionArn(scope);
	const image = config.PackageType === "Image";
	const role = resources.find(
		(r) => r.service === "iam" && r.type === "role" && r.arn === config.Role,
	);
	const [tab, setTab] = useState("code");
	const [dialog, setDialog] = useState<
		"code" | "configuration" | "tags" | "delete" | null
	>(null);
	const [settings, setSettings] = useState(settingsDefaults);
	const [code, setCode] = useState(codeDefaults);
	const [text, setText] = useState("{}");
	const [revision, setRevision] = useState<string>();
	const open = (mode: "code" | "configuration" | "tags") => {
		setSettings(settingsDefaults(config));
		setCode({
			...codeDefaults(details.data?.Code?.ImageUri),
			source: image ? "image" : "zip",
		});
		setText(JSON.stringify(tags, null, 2));
		setRevision(config.RevisionId);
		setDialog(mode);
	};
	const refresh = (message?: string) => {
		details.reload();
		onRefresh();
		if (message) onChanged(message);
	};
	const plan = async (): Promise<ActionStep[]> => {
		if (dialog === "delete") {
			const mappings = await functionMappings(scope);
			return [
				...mappings.map((mapping) => ({
					label: `Delete event source mapping ${mapping.UUID}`,
					run: () =>
						lambdaCall(scope, "DeleteEventSourceMapping", {
							UUID: mapping.UUID,
						}),
				})),
				{
					label: "Delete function",
					run: () => functionCall(scope, "DeleteFunction"),
				},
			];
		}
		if (dialog === "configuration") {
			const input = configurationInput(settings, image);
			return [
				{
					label: "Update configuration",
					run: () =>
						functionCall(scope, "UpdateFunctionConfiguration", {
							...input,
							...(revision ? { RevisionId: revision } : {}),
						}),
				},
			];
		}
		if (dialog === "code") {
			const input = await deploymentCode(code);
			return [
				{
					label: "Deploy function code",
					run: () =>
						functionCall(scope, "UpdateFunctionCode", {
							...input,
							...(revision ? { RevisionId: revision } : {}),
						}),
				},
			];
		}
		const next = stringMap(text, "Tags");
		const removed = Object.keys(tags).filter((k) => !(k in next));
		return [
			...(removed.length
				? [
						{
							label: "Remove tags",
							run: () =>
								lambdaCall(scope, "UntagResource", {
									Resource: arn,
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
								lambdaCall(scope, "TagResource", { Resource: arn, Tags: next }),
						},
					]
				: []),
		];
	};
	const pairs = [
		{ label: "Runtime", value: config.Runtime ?? "Container image" },
		{ label: "Handler", value: config.Handler ?? "Defined by image" },
		{ label: "Memory", value: `${config.MemorySize ?? 128} MB` },
		{ label: "Timeout", value: `${config.Timeout ?? 3} seconds` },
		{
			label: "Execution role",
			value: role ? (
				<Link
					href={iamHref("role", scope.account, role.name)}
					onFollow={follow}
				>
					{role.name}
				</Link>
			) : (
				(config.Role ?? "—")
			),
		},
		{ label: "Description", value: config.Description || "—" },
	];
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description={config.Description}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh function"
								onClick={() => refresh()}
							/>
							<Button
								disabled={!enabled || !config.FunctionName}
								onClick={() => setTab("test")}
							>
								Test
							</Button>
							<Button
								disabled={!enabled || !config.FunctionName}
								onClick={() => setDialog("delete")}
							>
								Delete
							</Button>
						</SpaceBetween>
					}
				>
					{scope.functionName}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{!!details.error && (
					<Alert type="error" header="Cannot load function">
						{details.error}
					</Alert>
				)}
				{!loading && !resource && !config.FunctionName && !details.loading && (
					<Alert type="warning">
						This function was not found in the selected account and Region.
					</Alert>
				)}
				<Container
					header={
						<Header
							variant="h2"
							description="Manage the $LATEST deployment and its connections."
						>
							Function overview
						</Header>
					}
				>
					<KeyValuePairs
						columns={3}
						items={[
							{
								label: "Function ARN",
								value: (
									<CopyToClipboard
										variant="inline"
										textToCopy={arn}
										copySuccessText="ARN copied"
										copyErrorText="ARN failed to copy"
									/>
								),
							},
							{ label: "State", value: config.State ?? "—" },
							{ label: "Last update", value: config.LastUpdateStatus ?? "—" },
							{ label: "Package type", value: config.PackageType ?? "—" },
							{
								label: "Architecture",
								value: config.Architectures?.join(", ") ?? "—",
							},
							{ label: "Last modified", value: config.LastModified ?? "—" },
						]}
					/>
				</Container>
				<Tabs
					activeTabId={tab}
					onChange={({ detail }) => setTab(detail.activeTabId)}
					tabs={[
						{
							id: "code",
							label: "Code",
							content: (
								<SpaceBetween size="l">
									<Container
										header={
											<Header
												variant="h2"
												actions={
													<Button
														variant="primary"
														disabled={!enabled || !config.FunctionName}
														onClick={() => open("code")}
													>
														Upload code
													</Button>
												}
											>
												Code source
											</Header>
										}
									>
										<SpaceBetween size="l">
											<Alert type="info">
												Deploy{" "}
												{image
													? "a container image"
													: "a ZIP archive or an S3 object"}{" "}
												to replace $LATEST. Executing the function requires
												Docker on the TinyStack host.
											</Alert>
											<KeyValuePairs
												columns={2}
												items={[
													{
														label: "Deployment size",
														value: `${config.CodeSize ?? 0} bytes`,
													},
													{ label: "SHA-256", value: config.CodeSha256 ?? "—" },
													...(image
														? [
																{
																	label: "Image URI",
																	value: details.data?.Code?.ImageUri ?? "—",
																},
															]
														: []),
												]}
											/>
										</SpaceBetween>
									</Container>
									<Container
										header={
											<Header
												variant="h2"
												actions={
													<Button
														disabled={!enabled || !config.FunctionName}
														onClick={() => open("configuration")}
													>
														Edit
													</Button>
												}
											>
												Runtime settings
											</Header>
										}
									>
										<KeyValuePairs columns={3} items={pairs.slice(0, 4)} />
									</Container>
								</SpaceBetween>
							),
						},
						{
							id: "test",
							label: "Test",
							content: (
								<LambdaTest
									scope={scope}
									enabled={enabled && !!config.FunctionName}
									onInvoked={() => refresh()}
								/>
							),
						},
						{
							id: "configuration",
							label: "Configuration",
							content: (
								<SpaceBetween size="l">
									<Container
										header={
											<Header
												variant="h2"
												actions={
													<Button
														disabled={!enabled || !config.FunctionName}
														onClick={() => open("configuration")}
													>
														Edit
													</Button>
												}
											>
												General configuration
											</Header>
										}
									>
										<KeyValuePairs columns={2} items={pairs} />
									</Container>
									<Container
										header={<Header variant="h2">Environment variables</Header>}
									>
										<CodeBlock
											text={JSON.stringify(
												config.Environment?.Variables ?? {},
												null,
												2,
											)}
										/>
									</Container>
									<Container
										header={
											<Header
												variant="h2"
												counter={`(${Object.keys(tags).length})`}
												actions={
													<Button
														disabled={!enabled || !config.FunctionName}
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
										<CodeBlock text={JSON.stringify(tags, null, 2)} />
									</Container>
									{config.Layers?.length ? (
										<Container header={<Header variant="h2">Layers</Header>}>
											<SpaceBetween size="s">
												{config.Layers.map((layer) => (
													<Box key={layer.Arn}>{layer.Arn}</Box>
												))}
											</SpaceBetween>
										</Container>
									) : null}
								</SpaceBetween>
							),
						},
						{
							id: "triggers",
							label: "Triggers",
							content: (
								<LambdaTriggers
									scope={scope}
									resources={resources}
									enabled={enabled && !!config.FunctionName}
									snsEnabled={snsEnabled && !!config.FunctionName}
									loading={loading}
									onRefresh={onRefresh}
									onChanged={(message) => refresh(message)}
								/>
							),
						},
						{
							id: "urls",
							label: "Function URL",
							content: (
								<LambdaUrls
									scope={scope}
									enabled={enabled && !!config.FunctionName}
									loading={loading}
									onRefresh={onRefresh}
									onChanged={(message) => refresh(message)}
								/>
							),
						},
						{
							id: "monitor",
							label: "Monitor",
							content: <LambdaLogs scope={scope} enabled={logsEnabled} />,
						},
					]}
				/>
				{dialog && (
					<ActionDialog
						title={
							dialog === "delete"
								? "Delete function"
								: dialog === "code"
									? "Upload function code"
									: dialog === "tags"
										? "Edit tags"
										: "Edit configuration"
						}
						submit={
							dialog === "delete"
								? "Delete function"
								: dialog === "code"
									? "Deploy"
									: "Save changes"
						}
						confirmation={dialog === "delete" ? scope.functionName : undefined}
						plan={plan}
						onClose={() => setDialog(null)}
						onSaved={() => {
							setDialog(null);
							if (dialog === "delete") onDeleted();
							else
								refresh(
									dialog === "code"
										? "Function code deployed."
										: dialog === "tags"
											? "Function tags updated."
											: "Function configuration updated.",
								);
						}}
					>
						{dialog === "delete" ? (
							<Alert type="warning">
								Delete {scope.functionName}, its versions, aliases and event
								source mappings. CloudWatch logs and SNS subscriptions remain;
								remove subscriptions from their topics separately.
							</Alert>
						) : dialog === "code" ? (
							<LambdaCodeInput
								scope={scope}
								resources={resources}
								image={image}
								value={code}
								onChange={setCode}
							/>
						) : dialog === "configuration" ? (
							<LambdaSettings
								scope={scope}
								resources={resources}
								image={image}
								value={settings}
								onChange={setSettings}
							/>
						) : (
							<FormField
								label="Tags (JSON)"
								description="Object of string values. Use {} to remove all tags."
							>
								<Textarea
									ariaLabel="Function tags"
									rows={8}
									value={text}
									onChange={({ detail }) => setText(detail.value)}
								/>
							</FormField>
						)}
					</ActionDialog>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}
