import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, PickMany, Text } from "#/components/memorydb/Fields";
import {
	type ACL,
	type MemoryDBScope,
	memorydbCall,
	type User,
} from "#/lib/memorydb";
import {
	authSummary,
	createUserInput,
	defaultUserName,
	maskSecret,
	openAccessACL,
	updateACLInput,
	updateUserInput,
	validName,
} from "#/lib/memorydbHelpers";

type UserDialog = "" | "create" | "update" | "delete";

export function MemoryDBUsers({
	scope,
	enabled,
	users,
	loading,
	onChanged,
}: {
	scope: MemoryDBScope;
	enabled: boolean;
	users: User[];
	loading: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<UserDialog>(""),
		[target, setTarget] = useState<User>(),
		[name, setName] = useState(""),
		[access, setAccess] = useState("on ~* +@read"),
		[auth, setAuth] = useState<"password" | "iam">("password"),
		[passwords, setPasswords] = useState(["", ""]),
		[changeAuth, setChangeAuth] = useState(false);
	const open = (kind: UserDialog, user?: User) => {
		setTarget(user);
		setName(user?.Name ?? "");
		setAccess(user?.AccessString ?? "on ~* +@read");
		setAuth(user?.Authentication?.Type === "iam" ? "iam" : "password");
		setPasswords(["", ""]);
		setChangeAuth(false);
		setDialog(kind);
	};
	const editable = (u: User) => u.Name !== defaultUserName;
	const showAuth = dialog === "create" || changeAuth;
	return (
		<>
			<Rows
				title="Users"
				emptyText="Choose Create user to add one."
				items={users}
				loading={loading}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Create user
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (u) => u.Name },
					{ id: "status", header: "Status", cell: (u) => u.Status },
					{
						id: "auth",
						header: "Authentication",
						cell: (u) => authSummary(u.Authentication),
					},
					{
						id: "access",
						header: "Access string",
						cell: (u) => u.AccessString,
					},
					{
						id: "acls",
						header: "ACLs",
						cell: (u) => (u.ACLNames ?? []).join(", ") || "-",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (u) =>
							editable(u) ? (
								<Buttons>
									<Action disabled={!enabled} onClick={() => open("update", u)}>
										Update
									</Action>
									<Action disabled={!enabled} onClick={() => open("delete", u)}>
										Delete
									</Action>
								</Buttons>
							) : (
								"Built in"
							),
					},
				]}
			/>
			{(dialog === "create" || dialog === "update") && (
				<ActionDialog
					title={dialog === "create" ? "Create user" : `Update ${name}`}
					submit={dialog === "create" ? "Create user" : "Update user"}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => {
						const form = { name, access, auth, passwords };
						const input =
							dialog === "create" || !target
								? createUserInput(form)
								: updateUserInput(
										target.Name,
										form,
										target.AccessString ?? "",
										changeAuth,
									);
						const operation = dialog === "create" ? "CreateUser" : "UpdateUser";
						return [
							{
								label: operation,
								run: () => memorydbCall(scope, operation, input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Text
							label="User name"
							value={name}
							disabled={dialog === "update"}
							onChange={setName}
							constraint="1 to 40 letters, digits or hyphens; begins with a letter."
						/>
						<Text
							label="Access string"
							value={access}
							onChange={setAccess}
							description="Redis ACL rules, such as on ~app:* +@read. The reset command is not allowed."
						/>
						{dialog === "update" && (
							<Checkbox
								checked={changeAuth}
								onChange={({ detail }) => setChangeAuth(detail.checked)}
								description="Passwords are never shown; leave this off to keep the current ones."
							>
								Change authentication
							</Checkbox>
						)}
						{showAuth && (
							<>
								<Pick
									label="Authentication type"
									value={auth}
									placeholder="Choose a type"
									options={[{ value: "password" }, { value: "iam" }]}
									onChange={(v) => setAuth(v === "iam" ? "iam" : "password")}
								/>
								{auth === "password" &&
									passwords.map((p, i) => (
										<Text
											// biome-ignore lint/suspicious/noArrayIndexKey: two fixed slots
											key={i}
											label={
												i === 0 ? "Password" : "Second password (optional)"
											}
											type="password"
											value={p}
											onChange={(v) =>
												setPasswords((all) =>
													all.map((old, j) => (j === i ? v : old)),
												)
											}
											constraint={
												i === 0
													? '16 to 128 printable characters without , " / or @.'
													: p
														? `Entered: ${maskSecret(p)}`
														: undefined
											}
										/>
									))}
							</>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && target && (
				<ActionDialog
					title="Delete user"
					submit="Delete user"
					confirmation={target.Name}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteUser",
							run: () =>
								memorydbCall(scope, "DeleteUser", { UserName: target.Name }),
						},
					]}
				>
					<Alert type="warning">
						Deleting {target.Name} also removes it from its ACLs.
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}

type ACLDialog = "" | "create" | "update" | "delete";

export function MemoryDBACLs({
	scope,
	enabled,
	acls,
	users,
	loading,
	onChanged,
}: {
	scope: MemoryDBScope;
	enabled: boolean;
	acls: ACL[];
	users: User[];
	loading: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<ACLDialog>(""),
		[target, setTarget] = useState<ACL>(),
		[name, setName] = useState(""),
		[members, setMembers] = useState<string[]>([]);
	const open = (kind: ACLDialog, acl?: ACL) => {
		setTarget(acl);
		setName(acl?.Name ?? "");
		setMembers(acl?.UserNames ?? []);
		setDialog(kind);
	};
	const candidates = users
		.filter((u) => u.Name !== defaultUserName)
		.map((u) => ({ value: u.Name }));
	return (
		<>
			<Rows
				title="Access control lists"
				emptyText="Choose Create ACL to add one."
				items={acls}
				loading={loading}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Create ACL
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (a) => a.Name },
					{ id: "status", header: "Status", cell: (a) => a.Status },
					{
						id: "users",
						header: "Users",
						cell: (a) => (a.UserNames ?? []).join(", ") || "-",
					},
					{
						id: "clusters",
						header: "Clusters",
						cell: (a) => (a.Clusters ?? []).join(", ") || "-",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (a) =>
							a.Name === openAccessACL ? (
								"Built in"
							) : (
								<Buttons>
									<Action disabled={!enabled} onClick={() => open("update", a)}>
										Update members
									</Action>
									<Action disabled={!enabled} onClick={() => open("delete", a)}>
										Delete
									</Action>
								</Buttons>
							),
					},
				]}
			/>
			{(dialog === "create" || dialog === "update") && (
				<ActionDialog
					title={dialog === "create" ? "Create ACL" : `Update ${name}`}
					submit={dialog === "create" ? "Create ACL" : "Update ACL"}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => {
						if (dialog === "create") {
							const aclName = validName(name.trim(), "ACL name");
							return [
								{
									label: "CreateACL",
									run: () =>
										memorydbCall(scope, "CreateACL", {
											ACLName: aclName,
											...(members.length ? { UserNames: members } : {}),
										}),
								},
							];
						}
						const input = updateACLInput(
							target?.Name ?? name,
							target?.UserNames ?? [],
							members,
						);
						return [
							{
								label: "UpdateACL",
								run: () => memorydbCall(scope, "UpdateACL", input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Text
							label="ACL name"
							value={name}
							disabled={dialog === "update"}
							onChange={setName}
							constraint="1 to 40 letters, digits or hyphens; begins with a letter."
						/>
						<PickMany
							label="Users"
							values={members}
							options={candidates}
							placeholder="Choose users"
							onChange={setMembers}
						/>
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && target && (
				<ActionDialog
					title="Delete ACL"
					submit="Delete ACL"
					confirmation={target.Name}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteACL",
							run: () =>
								memorydbCall(scope, "DeleteACL", { ACLName: target.Name }),
						},
					]}
				>
					<Alert type="warning">
						An ACL that a cluster uses cannot be deleted.
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}
