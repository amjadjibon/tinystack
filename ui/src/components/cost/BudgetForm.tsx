import { Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import {
	budgetName,
	configuration,
	costCall,
	type Definition,
	notification,
	spend,
	subscriberAddresses,
} from "#/lib/cost";
import { CostInput, CostJSON, CostSelect } from "./CostFields";

export function BudgetNotificationFields({
	value,
	onChange,
}: {
	value: NotificationValues;
	onChange: (value: NotificationValues) => void;
}) {
	const update = (key: keyof NotificationValues) => (next: string) =>
		onChange({ ...value, [key]: next });
	return (
		<SpaceBetween size="m">
			<CostSelect
				label="Notification type"
				value={value.type}
				options={["ACTUAL", "FORECASTED"]}
				onChange={update("type")}
			/>
			<CostSelect
				label="Comparison"
				value={value.comparison}
				options={["GREATER_THAN", "LESS_THAN", "EQUAL_TO"]}
				onChange={update("comparison")}
			/>
			<CostInput
				label="Threshold"
				value={value.threshold}
				onChange={update("threshold")}
			/>
			<CostSelect
				label="Threshold type"
				value={value.thresholdType}
				options={["PERCENTAGE", "ABSOLUTE_VALUE"]}
				onChange={update("thresholdType")}
			/>
			<CostJSON
				label="Email subscribers (one per line)"
				value={value.emails}
				onChange={update("emails")}
				description="Up to ten email addresses; at least one email or SNS subscriber is required."
			/>
			<CostInput
				label="SNS subscriber ARN (optional)"
				value={value.sns}
				onChange={update("sns")}
			/>
		</SpaceBetween>
	);
}
export type NotificationValues = {
	type: string;
	comparison: string;
	threshold: string;
	thresholdType: string;
	emails: string;
	sns: string;
};
export const defaultNotification: NotificationValues = {
	type: "ACTUAL",
	comparison: "GREATER_THAN",
	threshold: "80",
	thresholdType: "PERCENTAGE",
	emails: "",
	sns: "",
};
export function notificationInput(value: NotificationValues) {
	const emails = value.emails.trim()
		? subscriberAddresses(value.emails, "EMAIL")
		: [];
	if (emails.length > 10)
		throw new Error("A notification supports at most ten email subscribers.");
	const sns = value.sns.trim() ? subscriberAddresses(value.sns, "SNS") : [];
	if (sns.length > 1 || (!emails.length && !sns.length))
		throw new Error(
			"Provide up to one SNS subscriber and at least one subscriber overall.",
		);
	return {
		Notification: notification(
			value.type,
			value.comparison,
			value.threshold,
			value.thresholdType,
		),
		Subscribers: [
			...emails.map((Address) => ({ SubscriptionType: "EMAIL", Address })),
			...sns.map((Address) => ({ SubscriptionType: "SNS", Address })),
		],
	};
}
export function BudgetForm({
	account,
	resource,
	onClose,
	onSaved,
}: {
	account: string;
	resource?: Resource;
	onClose: () => void;
	onSaved: () => void;
}) {
	const config = resource ? configuration(resource) : {};
	const limit = config.BudgetLimit as
		| { Amount?: string; Unit?: string }
		| undefined;
	const [name, setName] = useState(resource?.name ?? "");
	const [type, setType] = useState("COST"),
		[time, setTime] = useState("MONTHLY");
	const [amount, setAmount] = useState(limit?.Amount ?? "100"),
		[unit, setUnit] = useState(limit?.Unit ?? "USD");
	const [notify, setNotify] = useState(false),
		[values, setValues] = useState(defaultNotification);
	return (
		<ActionDialog
			title={resource ? "Edit budget limit" : "Create budget"}
			submit={resource ? "Save changes" : "Create budget"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const Budget: Definition = resource
					? { ...config, BudgetLimit: spend(amount, unit) }
					: {
							BudgetName: budgetName(name),
							BudgetType: type,
							TimeUnit: time,
							BudgetLimit: spend(amount, unit),
						};
				if (resource) delete Budget.PlannedBudgetLimits;
				const input = resource
					? { AccountId: account, NewBudget: Budget }
					: {
							AccountId: account,
							Budget,
							NotificationsWithSubscribers: notify
								? [notificationInput(values)]
								: [],
						};
				return [
					{
						label: resource ? "Update budget" : "Create budget",
						run: () =>
							costCall(
								"budgets",
								account,
								resource ? "UpdateBudget" : "CreateBudget",
								input,
							),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!resource && (
					<SpaceBetween size="m">
						<CostInput label="Budget name" value={name} onChange={setName} />
						<CostSelect
							label="Budget type"
							value={type}
							options={["COST", "USAGE"]}
							onChange={setType}
						/>
						<CostSelect
							label="Time unit"
							value={time}
							options={["MONTHLY", "QUARTERLY", "ANNUALLY"]}
							onChange={setTime}
						/>
					</SpaceBetween>
				)}
				<CostInput label="Limit amount" value={amount} onChange={setAmount} />
				<CostInput label="Limit unit" value={unit} onChange={setUnit} />
				{!resource && (
					<SpaceBetween size="m">
						<Checkbox
							checked={notify}
							onChange={({ detail }) => setNotify(detail.checked)}
						>
							Add a notification
						</Checkbox>
						{notify && (
							<BudgetNotificationFields value={values} onChange={setValues} />
						)}
					</SpaceBetween>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
