import {
	Alert,
	Checkbox,
	FormField,
	Input,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Field, Rows } from "#/components/containers/Controls";
import { mqCall, mqList } from "#/lib/mq";
import {
	type Broker,
	type MQScope,
	type MQUser,
	userInput,
} from "#/lib/mqHelpers";
import { usePoll } from "#/lib/usePoll";
export function Users({
	scope,
	broker,
	docker,
	enabled,
}: {
	scope: MQScope;
	broker: Broker;
	docker: boolean;
	enabled: boolean;
}) {
	const poll = usePoll(
		() =>
			mqList<MQUser>(scope, "ListUsers", "Users", {
				BrokerId: broker.BrokerId,
			}),
		2000,
	);
	const [dialog, setDialog] = useState("");
	const [name, setName] = useState(""),
		[password, setPassword] = useState(""),
		[groups, setGroups] = useState(""),
		[access, setAccess] = useState(false);
	const [error, setError] = useState("");
	const edit = async (u: MQUser) => {
		try {
			const d = await mqCall<MQUser>(scope, "DescribeUser", {
				BrokerId: broker.BrokerId,
				Username: u.Username,
			});
			setName(u.Username);
			setGroups((d.Pending?.Groups ?? d.Groups ?? []).join(", "));
			setAccess(d.Pending?.ConsoleAccess ?? d.ConsoleAccess ?? false);
			setPassword("");
			setDialog("update");
		} catch (e) {
			setError(String(e));
		}
	};
	if (broker.EngineType === "RABBITMQ")
		return <Alert>Manage RabbitMQ users through its management API.</Alert>;
	return (
		<SpaceBetween size="m">
			<Alert>
				User changes apply on reboot. Passwords cannot be retrieved.{" "}
				{docker
					? "User mutations are unsupported in Docker mode; recreate with initial users."
					: "Reboot the broker after saving pending changes."}
			</Alert>
			{!!(poll.error || error) && (
				<Alert type="error">{poll.error || error}</Alert>
			)}
			<Rows
				title="Users"
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="No users"
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={docker || !enabled}
							onClick={() => {
								setName("");
								setPassword("");
								setGroups("");
								setAccess(false);
								setDialog("create");
							}}
						>
							Create user
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Username", cell: (u) => u.Username },
					{
						id: "pending",
						header: "Pending change",
						cell: (u) => u.PendingChange ?? "-",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (u) => (
							<Buttons>
								<Action
									disabled={docker || !enabled}
									onClick={() => void edit(u)}
								>
									Update
								</Action>
								<Action
									disabled={docker || !enabled}
									onClick={() => {
										setName(u.Username);
										setDialog("delete");
									}}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{!!dialog && (
				<ActionDialog
					title={`${dialog} user`}
					submit={`${dialog} user`}
					confirmation={dialog === "delete" ? name : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						setPassword("");
						poll.reload();
					}}
					plan={async () => {
						const op =
							dialog === "create"
								? "CreateUser"
								: dialog === "update"
									? "UpdateUser"
									: "DeleteUser";
						const input =
							dialog === "delete"
								? { Username: name }
								: userInput(
										name,
										password,
										groups,
										access,
										dialog === "update",
									);
						return [
							{
								label: op,
								run: () =>
									mqCall(scope, op, { BrokerId: broker.BrokerId, ...input }),
							},
						];
					}}
				>
					{dialog === "delete" ? (
						<Alert>Delete {name} on the next reboot.</Alert>
					) : (
						<SpaceBetween size="m">
							<Field
								label="Username"
								value={name}
								onChange={(v) => {
									if (dialog !== "update") setName(v);
								}}
							/>
							<FormField label="Password (leave empty to retain on update)">
								<Input
									type="password"
									value={password}
									onChange={({ detail }) => setPassword(detail.value)}
								/>
							</FormField>
							<Field label="Groups" value={groups} onChange={setGroups} />
							<Checkbox
								checked={access}
								onChange={({ detail }) => setAccess(detail.checked)}
							>
								Console access
							</Checkbox>
						</SpaceBetween>
					)}
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
