import {
	Alert,
	Button,
	Modal,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import {
	type CognitoScope,
	cognitoCall,
	cognitoPages,
	type Group,
} from "#/lib/cognito";
import type { CognitoUser } from "#/lib/cognitoHelpers";
import { errorText } from "#/lib/consoleActions";
import { usePoll } from "#/lib/usePoll";
import { Field } from "./CognitoDetails";
import { CognitoForm } from "./CognitoManagement";
import { DataTable } from "./CognitoTable";
export function CognitoGroups({
	scope,
	enabled,
}: {
	scope: CognitoScope;
	enabled: boolean;
}) {
	const groups = usePoll(() =>
		enabled
			? cognitoPages<Group>(scope, "ListGroups", "Groups")
			: Promise.resolve([]),
	);
	const [editing, setEditing] = useState<Group>();
	const [selected, setSelected] = useState(""),
		[mode, setMode] = useState(""),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[error, setError] = useState(""),
		[busy, setBusy] = useState(false);
	async function save() {
		setBusy(true);
		setError("");
		try {
			if (mode === "create")
				await cognitoCall(scope, "CreateGroup", {
					GroupName: name,
					Description: description,
				});
			else {
				await cognitoCall(scope, "DeleteGroup", { GroupName: name });
				setSelected("");
			}
			setMode("");
			setName("");
			setDescription("");
			groups.reload();
		} catch (e) {
			setError(errorText(e));
		} finally {
			setBusy(false);
		}
	}
	return (
		<SpaceBetween size="l">
			<DataTable
				title="Groups"
				poll={groups}
				actions={
					<Button
						disabled={!enabled}
						onClick={() => {
							setName("");
							setError("");
							setMode("create");
						}}
					>
						Create group
					</Button>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (g) => (
							<Button variant="link" onClick={() => setSelected(g.GroupName)}>
								{g.GroupName}
							</Button>
						),
					},
					{
						id: "description",
						header: "Description",
						cell: (g) => g.Description ?? "—",
					},
					{
						id: "delete",
						header: "Actions",
						cell: (g) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!enabled}
									onClick={async () => {
										try {
											setEditing(
												(
													await cognitoCall(scope, "GetGroup", {
														GroupName: g.GroupName,
													})
												).Group,
											);
										} catch (e) {
											setError(errorText(e));
										}
									}}
								>
									Edit
								</Button>
								<Button
									disabled={!enabled}
									onClick={() => {
										setName(g.GroupName);
										setError("");
										setMode("delete");
									}}
								>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
			/>
			{error && !mode && <Alert type="error">{error}</Alert>}
			{editing && (
				<CognitoForm
					title={`Edit group ${editing.GroupName}`}
					initial={{ ...editing }}
					fields={[
						{ key: "Description", label: "Description" },
						{ key: "Precedence", label: "Precedence", kind: "number" },
						{ key: "RoleArn", label: "Role ARN" },
					]}
					onClose={() => setEditing(undefined)}
					onSaved={() => {
						setEditing(undefined);
						groups.reload();
					}}
					save={(input) =>
						cognitoCall(scope, "UpdateGroup", {
							GroupName: editing.GroupName,
							Description: input.Description,
							Precedence: input.Precedence,
							...(input.RoleArn ? { RoleArn: input.RoleArn } : {}),
						})
					}
				/>
			)}
			{!!selected && (
				<GroupMembers
					key={selected}
					scope={scope}
					group={selected}
					enabled={enabled}
				/>
			)}
			<Modal
				visible={!!mode}
				onDismiss={() => setMode("")}
				header={mode === "create" ? "Create group" : `Delete group ${name}?`}
				footer={
					<SpaceBetween direction="horizontal" size="s">
						<Button disabled={busy} onClick={() => setMode("")}>
							Cancel
						</Button>
						<Button loading={busy} disabled={!enabled || !name} onClick={save}>
							{mode === "create" ? "Create group" : "Delete group"}
						</Button>
					</SpaceBetween>
				}
			>
				<SpaceBetween size="m">
					{error && <Alert type="error">{error}</Alert>}
					{mode === "create" ? (
						<SpaceBetween size="m">
							<Field label="Group name" value={name} onChange={setName} />
							<Field
								label="Description"
								value={description}
								onChange={setDescription}
							/>
						</SpaceBetween>
					) : (
						<div>Delete {name} from this pool?</div>
					)}
				</SpaceBetween>
			</Modal>
		</SpaceBetween>
	);
}
function GroupMembers({
	scope,
	group,
	enabled,
}: {
	scope: CognitoScope;
	group: string;
	enabled: boolean;
}) {
	const members = usePoll(() =>
		enabled
			? cognitoPages<CognitoUser>(scope, "ListUsersInGroup", "Users", {
					GroupName: group,
				})
			: Promise.resolve([]),
	);
	return (
		<DataTable
			title={`Members of ${group}`}
			poll={members}
			columns={[
				{ id: "name", header: "Username", cell: (u) => u.Username },
				{ id: "status", header: "Status", cell: (u) => u.UserStatus ?? "—" },
			]}
		/>
	);
}
