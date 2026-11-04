import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	CopyToClipboard,
	Flashbar,
	Header,
	KeyValuePairs,
	Modal,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import type { Resource } from "#/lib/api";
import {
	type IAMKind,
	iamCall,
	iamEntity,
	iamInput,
	iamLabels,
	policyText,
} from "#/lib/iam";
import { usePoll } from "#/lib/usePoll";
import { IAMAccessKeys } from "./IAMAccessKeys";
import type { IAMActions, IAMSecret } from "./IAMForm";
import { IAMActionDialog, type IAMDialog } from "./IAMForm";
import { IAMMembership } from "./IAMMembership";
import { IAMPermissions } from "./IAMPermissions";
import { IAMPolicyDocument, IAMPolicyVersions } from "./IAMPolicy";
import { IAMProfiles } from "./IAMProfiles";
import { IAMTags } from "./IAMTags";

export function IAMDetail({
	resource,
	resources,
	enabled,
	onRefresh,
	onNavigate,
	onDeleted,
}: {
	resource: Resource;
	resources: Resource[];
	enabled: boolean;
	onRefresh: () => void;
	onNavigate: (kind: IAMKind, name: string) => void;
	onDeleted: () => void;
}) {
	const kind = resource.type as IAMKind;
	const entity = iamEntity[kind];
	const [dialog, setDialog] = useState<IAMDialog>();
	const [notice, setNotice] = useState("");
	const [error, setError] = useState("");
	const [secret, setSecret] = useState<IAMSecret>();
	const live = usePoll(() =>
		enabled
			? iamCall(resource.account, `Get${entity}`, iamInput(resource))
			: Promise.resolve<Record<string, unknown>>({}),
	);
	const config = (live.data?.[entity] ?? {}) as Record<string, unknown>;
	const reload = () => {
		onRefresh();
		live.reload();
	};
	const run: IAMActions["run"] = async (operation, input = {}) => {
		let deleted = false;
		try {
			const output = await iamCall(resource.account, operation, {
				...(operation.includes("InstanceProfile") ? {} : iamInput(resource)),
				...input,
			});
			setError("");
			deleted = operation === `Delete${entity}`;
			return output;
		} finally {
			onRefresh();
			if (!deleted) live.reload();
		}
	};
	const open: IAMActions["open"] = (next) => {
		setError("");
		setDialog(next);
	};
	const actions = { resource, enabled, run, open };
	const remove = () =>
		open({
			title: `Delete ${iamLabels[kind].singular.toLowerCase()}`,
			confirm: "Delete",
			description: `Delete ${resource.name}? Remove its policies, memberships, and credentials first. This action cannot be undone.`,
			fields: [
				{
					id: "confirm",
					label: `Type ${resource.name} to confirm`,
					required: true,
				},
			],
			submit: async (values) => {
				if (values.confirm !== resource.name)
					throw new Error("The name does not match.");
				await run(`Delete${entity}`);
				onDeleted();
			},
		});
	const edit = () =>
		open(
			kind === "role"
				? {
						title: "Edit role",
						fields: [
							{ id: "Description", label: "Description" },
							{
								id: "MaxSessionDuration",
								label: "Maximum session duration (seconds)",
								type: "number",
								required: true,
							},
						],
						values: {
							Description: String(config.Description ?? ""),
							MaxSessionDuration: String(config.MaxSessionDuration ?? 3600),
						},
						submit: async (values) => {
							await run("UpdateRole", {
								Description: values.Description,
								MaxSessionDuration: Number(values.MaxSessionDuration),
							});
						},
					}
				: {
						title: `Edit ${iamLabels[kind].singular.toLowerCase()}`,
						fields: [
							{
								id: "Name",
								label: `${iamLabels[kind].singular} name`,
								required: true,
							},
							{ id: "Path", label: "Path", required: true },
						],
						values: {
							Name: resource.name,
							Path: String(config.Path ?? resource.details?.Path ?? "/"),
						},
						submit: async (values) => {
							await run(`Update${entity}`, {
								[`New${entity}Name`]: values.Name,
								NewPath: values.Path,
							});
							onNavigate(kind, values.Name);
						},
					},
		);
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description={`${iamLabels[kind].singular} · Account ${resource.account}`}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh IAM resource"
								onClick={reload}
							/>
							{kind !== "policy" && (
								<Button
									disabled={!enabled || live.loading || !!live.error}
									onClick={edit}
								>
									Edit
								</Button>
							)}
							<Button disabled={!enabled} onClick={remove}>
								Delete
							</Button>
						</SpaceBetween>
					}
				>
					{resource.name}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{!!notice && (
					<Flashbar
						items={[
							{
								id: "iam-detail-success",
								type: "success",
								content: notice,
								dismissible: true,
								onDismiss: () => setNotice(""),
							},
						]}
					/>
				)}
				{!!(error || live.error) && (
					<Alert type="error">{error || live.error}</Alert>
				)}
				<Container header={<Header variant="h2">Summary</Header>}>
					<KeyValuePairs
						columns={3}
						items={[
							{
								label: "ARN",
								value: (
									<CopyToClipboard
										variant="inline"
										textToCopy={resource.arn}
										copySuccessText="ARN copied"
										copyErrorText="Copy failed"
									/>
								),
							},
							{
								label: "Path",
								value: String(config.Path ?? resource.details?.Path ?? "/"),
							},
							{
								label: "Created",
								value: config.CreateDate
									? new Date(String(config.CreateDate)).toLocaleString()
									: "—",
							},
							...(kind === "role"
								? [
										{
											label: "Description",
											value: String(config.Description ?? "—"),
										},
										{
											label: "Maximum session duration",
											value: `${config.MaxSessionDuration ?? "—"} seconds`,
										},
									]
								: []),
							...(kind === "policy"
								? [
										{
											label: "Description",
											value: String(config.Description ?? "—"),
										},
										{
											label: "Attached entities",
											value: String(resource.details?.AttachmentCount ?? 0),
										},
									]
								: []),
						]}
					/>
				</Container>
				<Tabs
					tabs={[
						...(kind !== "policy"
							? [
									{
										id: "permissions",
										label: "Permissions",
										content: (
											<IAMPermissions
												{...actions}
												resources={resources}
												onError={setError}
											/>
										),
									},
								]
							: [
									{
										id: "document",
										label: "Permissions",
										content: <IAMPolicyDocument {...actions} />,
									},
									{
										id: "versions",
										label: "Policy versions",
										content: (
											<IAMPolicyVersions {...actions} onError={setError} />
										),
									},
								]),
						...(kind === "user" || kind === "group"
							? [
									{
										id: "membership",
										label: kind === "user" ? "User groups" : "Users",
										content: (
											<IAMMembership {...actions} resources={resources} />
										),
									},
								]
							: []),
						...(kind === "role"
							? [
									{
										id: "profiles",
										label: "Instance profiles",
										content: <IAMProfiles {...actions} resources={resources} />,
									},
									{
										id: "trust",
										label: "Trust relationships",
										content: (
											<Container
												header={
													<Header
														variant="h2"
														actions={
															<Button
																disabled={
																	!enabled || live.loading || !!live.error
																}
																onClick={() =>
																	open({
																		title: "Edit trust policy",
																		fields: [
																			{
																				id: "Document",
																				label: "Trust policy",
																				type: "json",
																				required: true,
																			},
																		],
																		values: {
																			Document: policyText(
																				config.AssumeRolePolicyDocument ??
																					resource.details
																						?.AssumeRolePolicyDocument,
																			),
																		},
																		submit: async (values) => {
																			await run("UpdateAssumeRolePolicy", {
																				PolicyDocument: values.Document,
																			});
																		},
																	})
																}
															>
																Edit trust policy
															</Button>
														}
													>
														Trust policy
													</Header>
												}
											>
												<CodeBlock
													text={policyText(
														config.AssumeRolePolicyDocument ??
															resource.details?.AssumeRolePolicyDocument,
													)}
												/>
											</Container>
										),
									},
								]
							: []),
						...(kind === "user"
							? [
									{
										id: "credentials",
										label: "Security credentials",
										content: (
											<IAMAccessKeys {...actions} onSecret={setSecret} />
										),
									},
								]
							: []),
						...(kind !== "group"
							? [
									{
										id: "tags",
										label: "Tags",
										content: <IAMTags {...actions} />,
									},
								]
							: []),
					]}
				/>
			</SpaceBetween>
			{dialog && (
				<IAMActionDialog
					key={dialog.title + JSON.stringify(dialog.values)}
					dialog={dialog}
					account={resource.account}
					onClose={() => setDialog(undefined)}
					onSaved={() => {
						if (dialog.confirm !== "Close") setNotice("Changes saved.");
					}}
				/>
			)}
			{secret && (
				<Modal
					visible
					header="Retrieve access key"
					onDismiss={() => setSecret(undefined)}
					footer={
						<Box float="right">
							<Button variant="primary" onClick={() => setSecret(undefined)}>
								Done
							</Button>
						</Box>
					}
				>
					<SpaceBetween size="l">
						<Alert type="info">
							Copy the secret access key now. You cannot retrieve it after
							closing this dialog.
						</Alert>
						<KeyValuePairs
							items={[
								{
									label: "Access key ID",
									value: (
										<CopyToClipboard
											variant="inline"
											textToCopy={secret.AccessKeyId}
											copySuccessText="Access key ID copied"
											copyErrorText="Copy failed"
										/>
									),
								},
								{
									label: "Secret access key",
									value: (
										<CopyToClipboard
											variant="inline"
											textToCopy={secret.SecretAccessKey}
											copySuccessText="Secret copied"
											copyErrorText="Copy failed"
										/>
									),
								},
							]}
						/>
					</SpaceBetween>
				</Modal>
			)}
		</ContentLayout>
	);
}
