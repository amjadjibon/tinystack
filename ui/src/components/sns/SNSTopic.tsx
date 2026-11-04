import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	ContentLayout,
	CopyToClipboard,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	SpaceBetween,
	Tabs,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { pretty, type Resource } from "#/lib/api";
import { type ActionStep, jsonObject, stringMap } from "#/lib/consoleActions";
import {
	type SNSScope,
	snsCall,
	topicArn,
	topicCall,
	topicSubscriptions,
} from "#/lib/sns";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";
import { SNSDeliveries } from "./SNSDeliveries";
import { SNSPublish } from "./SNSPublish";
import { SNSSubscriptionForm } from "./SNSSubscriptionForm";
import { SNSSubscriptions } from "./SNSSubscriptions";

export function SNSTopic({
	scope,
	initialTab,
	resource,
	resources,
	enabled,
	inventoryLoading,
	onRefresh,
	onChanged,
	onDeleted,
}: {
	scope: SNSScope;
	/** initialTab opens a tab named in the URL (empty for the default). */
	initialTab: string;
	resource?: Resource;
	resources: Resource[];
	enabled: boolean;
	inventoryLoading: boolean;
	onRefresh: () => void;
	onChanged: (message: string) => void;
	onDeleted: () => void;
}) {
	const details = usePoll(async () =>
		enabled
			? ((
					await topicCall<{ Attributes?: Record<string, string> }>(
						scope,
						"GetTopicAttributes",
					)
				).Attributes ?? {})
			: ((resource?.details?.Attributes as Record<string, string>) ?? {}),
	);
	const tags = usePoll(async () =>
		enabled
			? Object.fromEntries(
					(
						(
							await snsCall<{ Tags?: { Key: string; Value: string }[] }>(
								scope,
								"ListTagsForResource",
								{ ResourceArn: topicArn(scope) },
							)
						).Tags ?? []
					).map((t) => [t.Key, t.Value]),
				)
			: (resource?.tags ?? {}),
	);
	const [tab, setTab] = useState(initialTab || "subscriptions");
	const [dialog, setDialog] = useState<
		"edit" | "delete" | "policy" | "tags" | "subscribe" | null
	>(null);
	const [displayName, setDisplayName] = useState("");
	const [dedup, setDedup] = useState(false);
	const [text, setText] = useState("");
	const attributes =
		details.data ??
		(resource?.details?.Attributes as Record<string, string>) ??
		{};
	const subscriptions = resource ? topicSubscriptions(resource) : [];
	const fifo =
		attributes.FifoTopic === "true" || scope.topic?.endsWith(".fifo") === true;
	const arn = topicArn(scope);
	const refresh = (message: string) => {
		details.reload();
		tags.reload();
		onChanged(message);
	};
	const open = (mode: "edit" | "policy" | "tags") => {
		setDisplayName(attributes.DisplayName ?? "");
		setDedup(attributes.ContentBasedDeduplication === "true");
		setText(
			mode === "tags"
				? JSON.stringify(tags.data ?? {}, null, 2)
				: pretty(attributes.Policy ?? "{}"),
		);
		setDialog(mode);
	};
	const setAttribute = (name: string, value: string): ActionStep => ({
		label: `Save ${name}`,
		run: () =>
			topicCall(scope, "SetTopicAttributes", {
				AttributeName: name,
				AttributeValue: value,
			}),
	});
	const plan = async (): Promise<ActionStep[]> => {
		if (dialog === "delete")
			return [
				{ label: "Delete topic", run: () => topicCall(scope, "DeleteTopic") },
			];
		if (dialog === "edit") {
			if (displayName.length > 100)
				throw new Error("Display name must be at most 100 characters.");
			return [
				setAttribute("DisplayName", displayName),
				...(fifo
					? [setAttribute("ContentBasedDeduplication", String(dedup))]
					: []),
			];
		}
		if (dialog === "policy") {
			jsonObject(text, "Access policy");
			return [setAttribute("Policy", text.trim())];
		}
		const next = stringMap(text, "Tags");
		const removed = Object.keys(tags.data ?? {}).filter((k) => !(k in next));
		return [
			...(removed.length
				? [
						{
							label: "Remove tags",
							run: () =>
								snsCall(scope, "UntagResource", {
									ResourceArn: arn,
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
								snsCall(scope, "TagResource", {
									ResourceArn: arn,
									Tags: tagList(next),
								}),
						},
					]
				: []),
		];
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
								ariaLabel="Refresh topic"
								onClick={() => {
									details.reload();
									tags.reload();
									onRefresh();
								}}
							/>
							<Button
								disabled={!enabled || !details.data || !!details.error}
								onClick={() => open("edit")}
							>
								Edit
							</Button>
							<Button disabled={!enabled} onClick={() => setDialog("delete")}>
								Delete
							</Button>
							<Button
								variant="primary"
								disabled={!enabled}
								onClick={() => setTab("publish")}
							>
								Publish message
							</Button>
						</SpaceBetween>
					}
				>
					{scope.topic}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{(details.error || tags.error) && (
					<Alert type="error" header="Cannot load topic">
						{details.error || tags.error}
					</Alert>
				)}
				<Container header={<Header variant="h2">Details</Header>}>
					<KeyValuePairs
						columns={3}
						items={[
							{ label: "Name", value: scope.topic },
							{ label: "Type", value: fifo ? "FIFO" : "Standard" },
							{ label: "Display name", value: attributes.DisplayName || "—" },
							{
								label: "ARN",
								value: (
									<CopyToClipboard
										variant="inline"
										textToCopy={arn}
										copyButtonAriaLabel="Copy topic ARN"
										copySuccessText="Copied"
										copyErrorText="Copy failed"
									/>
								),
							},
							{ label: "AWS Region", value: scope.region },
							{ label: "Owner", value: scope.account },
							{
								label: "Confirmed subscriptions",
								value:
									attributes.SubscriptionsConfirmed ??
									subscriptions.filter((s) => !s.PendingConfirmation).length,
							},
							{
								label: "Pending subscriptions",
								value:
									attributes.SubscriptionsPending ??
									subscriptions.filter((s) => s.PendingConfirmation).length,
							},
							...(fifo
								? [
										{
											label: "Content-based deduplication",
											value:
												attributes.ContentBasedDeduplication === "true"
													? "Enabled"
													: "Disabled",
										},
									]
								: []),
						]}
					/>
				</Container>
				<Tabs
					activeTabId={tab}
					onChange={({ detail }) => setTab(detail.activeTabId)}
					tabs={[
						{
							id: "subscriptions",
							label: "Subscriptions",
							content: (
								<SNSSubscriptions
									subscriptions={subscriptions}
									loading={inventoryLoading}
									onRefresh={onRefresh}
									onCreate={enabled ? () => setDialog("subscribe") : undefined}
								/>
							),
						},
						{
							id: "publish",
							label: "Publish message",
							content: (
								<SNSPublish
									scope={scope}
									attributes={attributes}
									enabled={enabled}
									onPublished={() =>
										refresh("Message accepted for publication.")
									}
								/>
							),
						},
						{
							id: "policy",
							label: "Access policy",
							content: (
								<SpaceBetween size="l">
									<Alert type="info">
										TinyStack stores topic policies without evaluating them.
										Resources remain scoped to their account and Region.
									</Alert>
									<Container
										header={
											<Header
												variant="h2"
												actions={
													<Button
														disabled={
															!enabled || !details.data || !!details.error
														}
														onClick={() => open("policy")}
													>
														Edit
													</Button>
												}
											>
												Access policy
											</Header>
										}
									>
										<Textarea
											ariaLabel="Current topic policy"
											readOnly
											value={
												(attributes.Policy ? pretty(attributes.Policy) : "") ||
												"No policy in inventory. Use signed API requests to read the default policy in verify mode."
											}
											rows={14}
										/>
									</Container>
								</SpaceBetween>
							),
						},
						{
							id: "tags",
							label: "Tags",
							content: (
								<Container
									header={
										<Header
											variant="h2"
											counter={`(${Object.keys(tags.data ?? {}).length})`}
											actions={
												<Button
													disabled={!enabled || tags.loading || !!tags.error}
													onClick={() => open("tags")}
												>
													Manage tags
												</Button>
											}
										>
											Tags
										</Header>
									}
								>
									{Object.keys(tags.data ?? {}).length ? (
										<KeyValuePairs
											columns={2}
											items={Object.entries(tags.data ?? {}).map(
												([label, value]) => ({ label, value }),
											)}
										/>
									) : (
										<Box>No tags</Box>
									)}
								</Container>
							),
						},
						{
							id: "deliveries",
							label: "Deliveries",
							content: <SNSDeliveries arn={arn} />,
						},
					]}
				/>
			</SpaceBetween>
			{dialog === "subscribe" ? (
				<SNSSubscriptionForm
					scope={scope}
					resources={resources}
					fifo={fifo}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						refresh(
							"Subscription created. HTTP/HTTPS and email endpoints require confirmation.",
						);
					}}
				/>
			) : (
				dialog && (
					<ActionDialog
						title={
							dialog === "delete"
								? `Delete topic ${scope.topic}`
								: dialog === "edit"
									? "Edit topic"
									: dialog === "tags"
										? "Manage topic tags"
										: "Edit topic access policy"
						}
						submit={dialog === "delete" ? "Delete topic" : "Save changes"}
						confirmation={dialog === "delete" ? scope.topic : undefined}
						plan={plan}
						onClose={() => setDialog(null)}
						onSaved={() => {
							const mode = dialog;
							setDialog(null);
							if (mode === "delete") onDeleted();
							else refresh("Topic settings saved.");
						}}
					>
						{dialog === "delete" ? (
							<Alert type="warning">
								Permanently delete this topic and all its subscriptions.
								Existing messages already delivered to queues are retained.
							</Alert>
						) : dialog === "edit" ? (
							<SpaceBetween size="l">
								<FormField
									label="Display name"
									constraintText="Maximum 100 characters"
								>
									<Input
										ariaLabel="Edit topic display name"
										value={displayName}
										onChange={({ detail }) => setDisplayName(detail.value)}
									/>
								</FormField>
								{fifo && (
									<Checkbox
										checked={dedup}
										onChange={({ detail }) => setDedup(detail.checked)}
									>
										Content-based deduplication
									</Checkbox>
								)}
							</SpaceBetween>
						) : (
							<FormField
								label={
									dialog === "tags" ? "Tags (JSON)" : "Access policy (JSON)"
								}
								description={
									dialog === "tags"
										? "Object of string values. Omit a key to remove it."
										: "Enter a policy document with a Statement. Use an empty Statement array to remove grants."
								}
							>
								<Textarea
									ariaLabel="Topic settings JSON"
									value={text}
									rows={14}
									onChange={({ detail }) => setText(detail.value)}
								/>
							</FormField>
						)}
					</ActionDialog>
				)
			)}
		</ContentLayout>
	);
}
