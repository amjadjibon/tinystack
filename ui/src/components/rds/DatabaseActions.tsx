import {
	Alert,
	ButtonDropdown,
	Checkbox,
	FormField,
	Input,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import {
	type DBInstance,
	type RDSScope,
	rdsCall,
	validatePassword,
} from "#/lib/rds";
export function DatabaseActions({
	scope,
	instance,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: RDSScope;
	instance?: DBInstance;
	enabled: boolean;
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const [action, setAction] = useState(""),
		[password, setPassword] = useState(""),
		[confirm, setConfirm] = useState(""),
		[show, setShow] = useState(false),
		[dbClass, setClass] = useState("");
	const status = instance?.DBInstanceStatus,
		id = instance?.DBInstanceIdentifier ?? "";
	const available = enabled && status === "available";
	return (
		<>
			<ButtonDropdown
				items={[
					{ id: "Stop", text: "Stop", disabled: !available },
					{
						id: "Start",
						text: "Start",
						disabled: !enabled || status !== "stopped",
					},
					{ id: "Reboot", text: "Reboot", disabled: !available },
					{ id: "Modify", text: "Modify", disabled: !available },
					{
						id: "Delete",
						text: "Delete",
						disabled:
							!enabled ||
							!["available", "stopped", "failed"].includes(status ?? ""),
					},
				]}
				onItemClick={({ detail }) => {
					setAction(detail.id);
					setPassword("");
					setConfirm("");
					setShow(false);
					setClass(instance?.DBInstanceClass ?? "");
				}}
			>
				Actions
			</ButtonDropdown>
			{action && instance && (
				<ActionDialog
					title={`${action} ${id}`}
					submit={
						action === "Modify" ? "Apply immediately" : `${action} database`
					}
					confirmation={action === "Delete" ? id : undefined}
					onClose={() => setAction("")}
					onSaved={() => {
						setAction("");
						onChanged();
						if (action === "Delete") onDeleted();
					}}
					plan={async () => {
						const input: Record<string, unknown> = { DBInstanceIdentifier: id };
						if (action === "Delete") input.SkipFinalSnapshot = true;
						if (action === "Modify") {
							if (!dbClass.startsWith("db.") || dbClass.length < 4)
								throw new Error("Instance class must start with db.");
							input.DBInstanceClass = dbClass;
							input.ApplyImmediately = true;
							if (password) {
								validatePassword(password);
								if (password !== confirm)
									throw new Error("Passwords must match.");
								input.MasterUserPassword = password;
							} else if (confirm) throw new Error("Enter the new password.");
						}
						return [
							{
								label: `${action} database`,
								run: () => rdsCall(scope, `${action}DBInstance`, input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						{action === "Delete" ? (
							<Alert type="warning">
								This permanently deletes the database and its SQL data.
								SkipFinalSnapshot=true is required because snapshots are
								unsupported.
							</Alert>
						) : action === "Modify" ? (
							<>
								<Alert type="info">
									Changes apply immediately. Class changes affect metadata only.
									Leave the password blank to retain it.
								</Alert>
								<FormField label="DB instance class">
									<Input
										ariaLabel="DB instance class"
										value={dbClass}
										onChange={({ detail }) => setClass(detail.value)}
									/>
								</FormField>
								<FormField label="New master password">
									<Input
										ariaLabel="New master password"
										type={show ? "text" : "password"}
										value={password}
										onChange={({ detail }) => setPassword(detail.value)}
									/>
								</FormField>
								<FormField label="Confirm new password">
									<Input
										ariaLabel="Confirm new password"
										type={show ? "text" : "password"}
										value={confirm}
										onChange={({ detail }) => setConfirm(detail.value)}
									/>
								</FormField>
								<Checkbox
									checked={show}
									onChange={({ detail }) => setShow(detail.checked)}
								>
									Show passwords
								</Checkbox>
							</>
						) : (
							<Alert type="info">
								{action} database {id}? Existing SQL connections may be
								interrupted.
							</Alert>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
		</>
	);
}
