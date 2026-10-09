import {
	Alert,
	Autosuggest,
	Checkbox,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { pretty, type Resource } from "#/lib/api";
import { type ActionStep, jsonObject } from "#/lib/consoleActions";
import {
	type SNSScope,
	type SNSSubscription,
	snsCall,
	topicCall,
} from "#/lib/sns";

export function SNSSubscriptionForm({
	scope,
	resources,
	fifo,
	subscription,
	onClose,
	onSaved,
}: {
	scope: SNSScope;
	resources: Resource[];
	fifo: boolean;
	subscription?: SNSSubscription;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [protocol, setProtocol] = useState(subscription?.Protocol ?? "sqs");
	const [endpoint, setEndpoint] = useState(subscription?.Endpoint ?? "");
	const [raw, setRaw] = useState(subscription?.RawMessageDelivery ?? false);
	const [filter, setFilter] = useState(
		pretty(subscription?.FilterPolicy || "{}"),
	);
	const [filterScope, setFilterScope] = useState(
		subscription?.FilterPolicyScope || "MessageAttributes",
	);
	const protocols = (
		fifo
			? ["sqs"]
			: ["sqs", "lambda", "http", "https", "email", "email-json", "sms"]
	).map((value) => ({
		value,
		label:
			{
				sqs: "Amazon SQS",
				lambda: "AWS Lambda",
				http: "HTTP",
				https: "HTTPS",
				email: "Email",
				"email-json": "Email (JSON)",
				sms: "SMS",
			}[value] ?? value,
	}));
	const options = resources
		.filter(
			(r) =>
				r.account === scope.account &&
				((protocol === "sqs" &&
					r.service === "sqs" &&
					r.type === "queue" &&
					(fifo || r.details?.FifoQueue !== "true")) ||
					(protocol === "lambda" &&
						r.service === "lambda" &&
						r.type === "function")),
		)
		.map((r) => ({ value: r.arn, label: `${r.name} (${r.region})` }));
	const supportsRaw = ["sqs", "http", "https"].includes(protocol);
	const plan = async (): Promise<ActionStep[]> => {
		if (!endpoint.trim()) throw new Error("Enter a subscription endpoint.");
		const policy = JSON.stringify(jsonObject(filter, "Filter policy"));
		const attributes = {
			RawMessageDelivery: String(supportsRaw && raw),
			FilterPolicyScope: filterScope,
			FilterPolicy: policy,
		};
		if (!subscription)
			return [
				{
					label: "Create subscription",
					run: () =>
						topicCall(scope, "Subscribe", {
							Protocol: protocol,
							Endpoint: endpoint.trim(),
							Attributes: attributes,
							ReturnSubscriptionArn: true,
						}),
				},
			];
		const set = (
			AttributeName: string,
			AttributeValue: string,
		): ActionStep => ({
			label: `Save ${AttributeName}`,
			run: () =>
				snsCall(scope, "SetSubscriptionAttributes", {
					SubscriptionArn: subscription.SubscriptionArn,
					AttributeName,
					AttributeValue,
				}),
		});
		const scopeChanged = filterScope !== subscription.FilterPolicyScope;
		const oldPolicy = JSON.stringify(
			jsonObject(subscription.FilterPolicy || "{}", "Current filter policy"),
		);
		return [
			...(scopeChanged && subscription.FilterPolicy
				? [set("FilterPolicy", "{}")]
				: []),
			...(scopeChanged ? [set("FilterPolicyScope", filterScope)] : []),
			...(scopeChanged || oldPolicy !== policy
				? [set("FilterPolicy", policy)]
				: []),
			...(subscription.RawMessageDelivery !== (supportsRaw && raw)
				? [set("RawMessageDelivery", String(supportsRaw && raw))]
				: []),
		];
	};
	return (
		<ActionDialog
			title={subscription ? "Edit subscription" : "Create subscription"}
			submit={subscription ? "Save changes" : "Create subscription"}
			plan={plan}
			onClose={onClose}
			onSaved={onSaved}
		>
			<SpaceBetween size="l">
				<FormField label="Topic ARN">
					<Input
						readOnly
						value={`arn:aws:sns:${scope.region}:${scope.account}:${scope.topic}`}
					/>
				</FormField>
				<FormField label="Protocol">
					<Select
						ariaLabel="Subscription protocol"
						disabled={!!subscription}
						selectedOption={
							protocols.find((p) => p.value === protocol) ?? {
								label: protocol,
								value: protocol,
							}
						}
						options={protocols}
						onChange={({ detail }) => {
							setProtocol(detail.selectedOption.value ?? "sqs");
							setEndpoint("");
							setRaw(false);
						}}
					/>
				</FormField>
				<FormField
					label="Endpoint"
					description={
						protocol === "sqs" || protocol === "lambda"
							? "Enter an ARN or select an existing resource in this account."
							: protocol === "http" || protocol === "https"
								? "The endpoint receives a confirmation request before notifications."
								: protocol === "sms"
									? "Enter an E.164 phone number, for example +15551234567."
									: "Enter an email address. Confirmation is recorded in the delivery log."
					}
				>
					<Autosuggest
						ariaLabel="Subscription endpoint"
						value={endpoint}
						disabled={!!subscription}
						options={options}
						onChange={({ detail }) => setEndpoint(detail.value)}
						enteredTextLabel={(value) => `Use: ${value}`}
						empty="No matching resources"
					/>
				</FormField>
				{supportsRaw && (
					<Checkbox
						checked={raw}
						onChange={({ detail }) => setRaw(detail.checked)}
					>
						Enable raw message delivery (omit the SNS envelope)
					</Checkbox>
				)}
				{protocol === "email" ||
				protocol === "email-json" ||
				protocol === "sms" ? (
					<Alert type="info">
						TinyStack records email and SMS deliveries locally. It does not send
						them to providers.
					</Alert>
				) : null}
				<FormField label="Subscription filter policy scope">
					<Select
						ariaLabel="Filter policy scope"
						selectedOption={{
							value: filterScope,
							label:
								filterScope === "MessageBody"
									? "Message body"
									: "Message attributes",
						}}
						options={[
							{ value: "MessageAttributes", label: "Message attributes" },
							{ value: "MessageBody", label: "Message body" },
						]}
						onChange={({ detail }) =>
							setFilterScope(detail.selectedOption.value ?? "MessageAttributes")
						}
					/>
				</FormField>
				<FormField
					label="Subscription filter policy (JSON)"
					description='Use {} to receive all messages. For example {"event":["order-created"]}.'
				>
					<Textarea
						ariaLabel="Subscription filter policy"
						value={filter}
						rows={8}
						onChange={({ detail }) => setFilter(detail.value)}
					/>
				</FormField>
				{subscription && (
					<Alert type="info">
						Changes save in sequence. Switching filter scope clears the previous
						filter before saving its replacement. Completed changes remain if a
						later step fails.
					</Alert>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
