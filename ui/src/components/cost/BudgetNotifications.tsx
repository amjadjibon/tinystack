import {
	Button,
	Container,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import {
	type BudgetNotification,
	costCall,
	type Definition,
	notifications,
	subscriberAddresses,
} from "#/lib/cost";
import {
	BudgetNotificationFields,
	defaultNotification,
	notificationInput,
} from "./BudgetForm";
import { CostInput, CostSelect } from "./CostFields";
export function BudgetNotifications({
	account,
	resource,
	enabled,
	onChanged,
}: {
	account: string;
	resource: Resource;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<{
		title: string;
		input?: Definition;
		operation?: string;
		notification?: Definition;
	}>();
	const [values, setValues] = useState(defaultNotification);
	const [type, setType] = useState("EMAIL"),
		[address, setAddress] = useState("");
	const call = (operation: string, input: Definition) =>
		costCall("budgets", account, operation, {
			AccountId: account,
			BudgetName: resource.name,
			...input,
		});
	return (
		<Container
			header={
				<Header
					variant="h2"
					actions={
						<Button
							disabled={!enabled || notifications(resource).length >= 5}
							onClick={() => {
								setValues(defaultNotification);
								setDialog({ title: "Add notification" });
							}}
						>
							Add notification
						</Button>
					}
				>
					Notifications and subscribers
				</Header>
			}
		>
			<SpaceBetween size="l">
				{notifications(resource).map((row: BudgetNotification) => {
					const n = row.Notification;
					return (
						<Container
							key={JSON.stringify(n)}
							header={
								<Header
									variant="h3"
									actions={
										<SpaceBetween direction="horizontal" size="xs">
											<Button
												disabled={!enabled}
												onClick={() => {
													setAddress("");
													setDialog({
														title: "Add subscriber",
														notification: n,
													});
												}}
											>
												Add subscriber
											</Button>
											<Button
												disabled={!enabled}
												onClick={() =>
													setDialog({
														title: "Remove notification",
														operation: "DeleteNotification",
														input: { Notification: n },
													})
												}
											>
												Remove notification
											</Button>
										</SpaceBetween>
									}
								>{`${n.NotificationType} ${n.ComparisonOperator} ${n.Threshold} ${n.ThresholdType}`}</Header>
							}
						>
							<Table
								items={row.Subscribers ?? []}
								trackBy="Address"
								columnDefinitions={[
									{
										id: "type",
										header: "Type",
										cell: (s) => s.SubscriptionType,
									},
									{ id: "address", header: "Address", cell: (s) => s.Address },
									{
										id: "remove",
										header: "Actions",
										cell: (s) => (
											<Button
												disabled={!enabled}
												onClick={() =>
													setDialog({
														title: "Remove subscriber",
														operation: "DeleteSubscriber",
														input: { Notification: n, Subscriber: s },
													})
												}
											>
												Remove subscriber
											</Button>
										),
									},
								]}
								empty="No subscribers"
							/>
						</Container>
					);
				})}
				{!notifications(resource).length && "No notifications"}
			</SpaceBetween>
			{dialog && (
				<ActionDialog
					title={dialog.title}
					submit={dialog.title}
					onClose={() => setDialog(undefined)}
					onSaved={() => {
						setDialog(undefined);
						onChanged();
					}}
					plan={async () => {
						if (dialog.operation)
							return [
								{
									label: dialog.title,
									run: () => call(dialog.operation ?? "", dialog.input ?? {}),
								},
							];
						if (dialog.notification) {
							const addresses = subscriberAddresses(address, type);
							if (addresses.length !== 1)
								throw new Error("Provide one subscriber address.");
							return [
								{
									label: "Add subscriber",
									run: () =>
										call("CreateSubscriber", {
											Notification: dialog.notification,
											Subscriber: {
												SubscriptionType: type,
												Address: addresses[0],
											},
										}),
								},
							];
						}
						const input = notificationInput(values);
						return [
							{
								label: "Add notification",
								run: () => call("CreateNotification", input),
							},
						];
					}}
				>
					{dialog.operation ? (
						"This removes the selected definition."
					) : dialog.notification ? (
						<SpaceBetween size="m">
							<CostSelect
								label="Subscriber type"
								value={type}
								options={["EMAIL", "SNS"]}
								onChange={setType}
							/>
							<CostInput
								label="Subscriber address"
								value={address}
								onChange={setAddress}
							/>
						</SpaceBetween>
					) : (
						<BudgetNotificationFields value={values} onChange={setValues} />
					)}
				</ActionDialog>
			)}
		</Container>
	);
}
