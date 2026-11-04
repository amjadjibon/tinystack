import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	CopyToClipboard,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import {
	type SNSScope,
	type SNSSubscription as SNSSubscriptionData,
	snsCall,
	topicArn,
	topicCall,
} from "#/lib/sns";
import { usePoll } from "#/lib/usePoll";
import { SNSSubscriptionForm } from "./SNSSubscriptionForm";

export function SNSSubscription({
	scope,
	arn,
	subscription,
	resources,
	fifo,
	enabled,
	onChanged,
	onDeleted,
	onConfirmed,
}: {
	scope: SNSScope;
	arn: string;
	subscription?: SNSSubscriptionData;
	resources: Resource[];
	fifo: boolean;
	enabled: boolean;
	onChanged: (message: string) => void;
	onDeleted: () => void;
	onConfirmed: (arn: string) => void;
}) {
	const details = usePoll(async () =>
		enabled
			? ((
					await snsCall<{ Attributes?: Record<string, string> }>(
						scope,
						"GetSubscriptionAttributes",
						{ SubscriptionArn: arn },
					)
				).Attributes ?? {})
			: {},
	);
	const [dialog, setDialog] = useState<
		"edit" | "confirm" | "delete" | "resend" | null
	>(null);
	const [token, setToken] = useState("");
	const confirmed = useRef("");
	const attributes = details.data ?? {};
	const current: SNSSubscriptionData = {
		SubscriptionArn: arn,
		TopicArn: topicArn(scope),
		Protocol: attributes.Protocol ?? subscription?.Protocol ?? "",
		Endpoint: attributes.Endpoint ?? subscription?.Endpoint ?? "",
		PendingConfirmation: attributes.PendingConfirmation
			? attributes.PendingConfirmation === "true"
			: (subscription?.PendingConfirmation ?? false),
		RawMessageDelivery: attributes.RawMessageDelivery
			? attributes.RawMessageDelivery === "true"
			: (subscription?.RawMessageDelivery ?? false),
		FilterPolicy: attributes.FilterPolicy ?? subscription?.FilterPolicy ?? "",
		FilterPolicyScope:
			attributes.FilterPolicyScope ??
			subscription?.FilterPolicyScope ??
			"MessageAttributes",
	};
	const refresh = (message: string) => {
		details.reload();
		onChanged(message);
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
								ariaLabel="Refresh subscription"
								onClick={details.reload}
							/>
							<Button
								disabled={!enabled || !details.data || !!details.error}
								onClick={() => setDialog("edit")}
							>
								Edit
							</Button>
							<Button
								disabled={!enabled || !current.PendingConfirmation}
								onClick={() => setDialog("resend")}
							>
								Request confirmation
							</Button>
							<Button
								disabled={!enabled || !current.PendingConfirmation}
								onClick={() => {
									setToken("");
									setDialog("confirm");
								}}
							>
								Confirm subscription
							</Button>
							<Button disabled={!enabled} onClick={() => setDialog("delete")}>
								Delete
							</Button>
						</SpaceBetween>
					}
				>
					Subscription details
				</Header>
			}
		>
			<SpaceBetween size="l">
				{!!details.error && (
					<Alert type="error" header="Cannot load subscription">
						{details.error}
					</Alert>
				)}
				{current.PendingConfirmation && (
					<Alert type="info">
						This subscription is pending confirmation and will not receive
						notifications. Enter the token from the endpoint’s confirmation
						request to confirm it.
					</Alert>
				)}
				<Container header={<Header variant="h2">Details</Header>}>
					<KeyValuePairs
						columns={2}
						items={[
							{
								label: "Subscription ARN",
								value: (
									<CopyToClipboard
										variant="inline"
										textToCopy={arn}
										copyButtonAriaLabel="Copy subscription ARN"
										copySuccessText="Copied"
										copyErrorText="Copy failed"
									/>
								),
							},
							{ label: "Topic ARN", value: topicArn(scope) },
							{ label: "Protocol", value: current.Protocol || "—" },
							{ label: "Endpoint", value: current.Endpoint || "—" },
							{
								label: "Status",
								value: current.PendingConfirmation
									? "Pending confirmation"
									: "Confirmed",
							},
							{
								label: "Raw message delivery",
								value: current.RawMessageDelivery ? "Enabled" : "Disabled",
							},
						]}
					/>
				</Container>
				<Container
					header={<Header variant="h2">Subscription filter policy</Header>}
				>
					<SpaceBetween size="m">
						<Box>
							Scope:{" "}
							{current.FilterPolicyScope === "MessageBody"
								? "Message body"
								: "Message attributes"}
						</Box>
						<Textarea
							ariaLabel="Current subscription filter policy"
							readOnly
							value={current.FilterPolicy || "{}"}
							rows={8}
						/>
					</SpaceBetween>
				</Container>
			</SpaceBetween>
			{dialog === "edit" ? (
				<SNSSubscriptionForm
					scope={scope}
					resources={resources}
					fifo={fifo}
					subscription={current}
					onClose={() => setDialog(null)}
					onSaved={() => {
						setDialog(null);
						refresh("Subscription settings updated.");
					}}
				/>
			) : (
				dialog && (
					<ActionDialog
						title={
							dialog === "confirm"
								? "Confirm subscription"
								: dialog === "resend"
									? "Request subscription confirmation"
									: "Delete subscription"
						}
						submit={
							dialog === "confirm"
								? "Confirm subscription"
								: dialog === "resend"
									? "Request confirmation"
									: "Delete subscription"
						}
						confirmation={dialog === "delete" ? "delete" : undefined}
						onClose={() => setDialog(null)}
						plan={async () => {
							if (dialog === "delete")
								return [
									{
										label: "Delete subscription",
										run: () =>
											snsCall(scope, "Unsubscribe", { SubscriptionArn: arn }),
									},
								];
							if (dialog === "resend")
								return [
									{
										label: "Request confirmation",
										run: () =>
											topicCall(scope, "Subscribe", {
												Protocol: current.Protocol,
												Endpoint: current.Endpoint,
												Attributes: {
													RawMessageDelivery: String(
														current.RawMessageDelivery,
													),
													FilterPolicy: current.FilterPolicy || "{}",
													FilterPolicyScope: current.FilterPolicyScope,
												},
												ReturnSubscriptionArn: true,
											}),
									},
								];
							if (!token.trim())
								throw new Error(
									"Enter the confirmation token from the endpoint’s message.",
								);
							const value = token.trim();
							return [
								{
									label: "Confirm subscription",
									run: async () => {
										const result = await topicCall<{ SubscriptionArn: string }>(
											scope,
											"ConfirmSubscription",
											{ Token: value },
										);
										confirmed.current = result.SubscriptionArn;
									},
								},
							];
						}}
						onSaved={() => {
							const mode = dialog;
							setDialog(null);
							if (mode === "delete") onDeleted();
							else if (mode === "confirm") {
								refresh("Subscription confirmed.");
								onConfirmed(confirmed.current);
							} else refresh("Subscription confirmation requested.");
						}}
					>
						{dialog === "confirm" ? (
							<FormField
								label="Confirmation token"
								description="Copy the token from the HTTP/HTTPS confirmation message, or the email confirmation recorded in Deliveries."
							>
								<Input
									ariaLabel="Subscription confirmation token"
									value={token}
									onChange={({ detail }) => setToken(detail.value)}
								/>
							</FormField>
						) : dialog === "resend" ? (
							<Alert type="info">
								Send a new confirmation request to {current.Endpoint}. The
								subscription remains pending until confirmed.
							</Alert>
						) : (
							<Alert type="warning">
								Remove the subscription to {current.Endpoint}. It will no longer
								receive topic messages.
							</Alert>
						)}
					</ActionDialog>
				)
			)}
		</ContentLayout>
	);
}
