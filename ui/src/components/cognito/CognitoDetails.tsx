import {
	Alert,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	Modal,
	SpaceBetween,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import {
	type CognitoScope,
	cognitoCall,
	cognitoPages,
	type Group,
} from "#/lib/cognito";
import {
	type CognitoUser,
	changedAttributes,
	matchesUser,
	removedAttributes,
} from "#/lib/cognitoHelpers";
import { errorText, stringMap } from "#/lib/consoleActions";
import { mailbox } from "#/lib/mailbox";
import { usePoll } from "#/lib/usePoll";
import { DataTable } from "./CognitoTable";

export function Field({
	label,
	value,
	onChange,
	password = false,
}: {
	label: string;
	value: string;
	onChange: (v: string) => void;
	password?: boolean;
}) {
	return (
		<FormField label={label}>
			<Input
				value={value}
				onChange={({ detail }) => onChange(detail.value)}
				type={password ? "password" : "text"}
				autoComplete="off"
			/>
		</FormField>
	);
}
export function CognitoUsers({
	scope,
	enabled,
	initialUser,
}: {
	scope: CognitoScope;
	enabled: boolean;
	initialUser: string;
}) {
	const users = usePoll(() =>
		enabled
			? cognitoPages<CognitoUser>(scope, "ListUsers", "Users")
			: Promise.resolve([]),
	);
	const [query, setQuery] = useState(""),
		[selected, setSelected] = useState(initialUser),
		[create, setCreate] = useState(false);
	const [username, setUsername] = useState(""),
		[password, setPassword] = useState(""),
		[email, setEmail] = useState(""),
		[error, setError] = useState(""),
		[busy, setBusy] = useState(false);
	const close = () => {
		setCreate(false);
		setPassword("");
		setError("");
	};
	async function save() {
		setBusy(true);
		setError("");
		try {
			await cognitoCall(scope, "AdminCreateUser", {
				Username: username,
				TemporaryPassword: password,
				UserAttributes: email ? [{ Name: "email", Value: email }] : [],
			});
			close();
			setUsername("");
			setEmail("");
			users.reload();
		} catch (e) {
			setError(errorText(e));
			setPassword("");
		} finally {
			setBusy(false);
		}
	}
	return (
		<SpaceBetween size="l">
			<TextFilter
				filteringText={query}
				onChange={({ detail }) => setQuery(detail.filteringText)}
				filteringAriaLabel="Search username, email or status"
				filteringPlaceholder="Search username, email or status"
			/>
			<DataTable
				title="Users"
				poll={{
					...users,
					data: users.data?.filter((u) => matchesUser(u, query)),
				}}
				actions={
					<Button disabled={!enabled} onClick={() => setCreate(true)}>
						Create user
					</Button>
				}
				columns={[
					{
						id: "name",
						header: "Username",
						cell: (u) => (
							<Button variant="link" onClick={() => setSelected(u.Username)}>
								{u.Username}
							</Button>
						),
					},
					{
						id: "email",
						header: "Email",
						cell: (u) =>
							u.Attributes?.find((a) => a.Name === "email")?.Value ?? "—",
					},
					{ id: "status", header: "Status", cell: (u) => u.UserStatus ?? "—" },
					{
						id: "enabled",
						header: "Access",
						cell: (u) => (u.Enabled ? "Enabled" : "Disabled"),
					},
				]}
			/>
			{!!selected && (
				<UserDetail
					key={selected}
					scope={scope}
					enabled={enabled}
					username={selected}
					onClose={() => setSelected("")}
					onChanged={users.reload}
				/>
			)}
			<Modal
				visible={create}
				onDismiss={close}
				header="Create user"
				footer={
					<SpaceBetween direction="horizontal" size="s">
						<Button onClick={close} disabled={busy}>
							Cancel
						</Button>
						<Button
							variant="primary"
							loading={busy}
							disabled={!enabled || !username || !password}
							onClick={save}
						>
							Create user
						</Button>
					</SpaceBetween>
				}
			>
				<SpaceBetween size="m">
					{!!error && <Alert type="error">{error}</Alert>}
					<Field label="Username" value={username} onChange={setUsername} />
					<Field label="Email (optional)" value={email} onChange={setEmail} />
					<Field
						label="Temporary password"
						password
						value={password}
						onChange={setPassword}
					/>
				</SpaceBetween>
			</Modal>
		</SpaceBetween>
	);
}
function UserDetail({
	scope,
	enabled,
	username,
	onClose,
	onChanged,
}: {
	scope: CognitoScope;
	enabled: boolean;
	username: string;
	onClose: () => void;
	onChanged: () => void;
}) {
	const user = usePoll(() =>
		enabled
			? cognitoCall(scope, "AdminGetUser", { Username: username })
			: Promise.resolve(undefined),
	);
	const groups = usePoll(() =>
		enabled
			? cognitoPages<Group>(scope, "AdminListGroupsForUser", "Groups", {
					Username: username,
				})
			: Promise.resolve([]),
	);
	const [mode, setMode] = useState(""),
		[text, setText] = useState(""),
		[permanent, setPermanent] = useState(false),
		[error, setError] = useState(""),
		[busy, setBusy] = useState(false);
	const input = { Username: username };
	async function run(action: () => Promise<unknown>, deleted = false) {
		setBusy(true);
		setError("");
		try {
			await action();
			setMode("");
			setText("");
			user.reload();
			groups.reload();
			onChanged();
			if (deleted) onClose();
		} catch (e) {
			setError(errorText(e));
			setText(mode === "password" ? "" : text);
		} finally {
			setBusy(false);
		}
	}
	const close = () => {
		setMode("");
		setText("");
		setError("");
	};
	return (
		<Container
			header={
				<Header actions={<Button onClick={onClose}>Close user</Button>}>
					{username}
				</Header>
			}
		>
			<SpaceBetween size="m">
				{!!(user.error || error) && (
					<Alert type="error">{user.error || error}</Alert>
				)}
				{user.loading ? (
					"Loading user"
				) : (
					<SpaceBetween size="m">
						<div>
							Status: {user.data?.UserStatus ?? "—"} ·{" "}
							{user.data?.Enabled ? "Enabled" : "Disabled"}
						</div>
						<CodeBlock
							text={JSON.stringify(user.data?.UserAttributes ?? [], null, 2)}
						/>
					</SpaceBetween>
				)}
				<SpaceBetween direction="horizontal" size="xs">
					<Button
						disabled={!enabled || busy || !user.data}
						onClick={() =>
							run(() => cognitoCall(scope, "AdminConfirmSignUp", input))
						}
					>
						Confirm
					</Button>
					<Button
						disabled={!enabled || busy || !user.data}
						onClick={() =>
							run(() =>
								cognitoCall(
									scope,
									user.data?.Enabled ? "AdminDisableUser" : "AdminEnableUser",
									input,
								),
							)
						}
					>
						{user.data?.Enabled ? "Disable" : "Enable"}
					</Button>
					<Button
						disabled={!enabled || busy || !user.data}
						onClick={() =>
							run(() => cognitoCall(scope, "AdminResetUserPassword", input))
						}
					>
						Send password reset code
					</Button>
					<Button
						disabled={!enabled || busy || !user.data}
						onClick={() => {
							setText("");
							setMode("password");
						}}
					>
						Set password
					</Button>
					<Button
						disabled={!enabled || busy || !user.data}
						onClick={() => {
							setText(
								JSON.stringify(
									Object.fromEntries(
										(user.data?.UserAttributes ?? [])
											.filter((a) => a.Name !== "sub")
											.map((a) => [a.Name, a.Value]),
									),
									null,
									2,
								),
							);
							setMode("attributes");
						}}
					>
						Edit attributes
					</Button>
					<Button
						disabled={!enabled || busy}
						onClick={() => {
							setText("");
							setMode("group");
						}}
					>
						Add to group
					</Button>
					<Button
						disabled={!enabled || busy || !user.data}
						onClick={() =>
							run(() => cognitoCall(scope, "AdminUserGlobalSignOut", input))
						}
					>
						Sign out everywhere
					</Button>
					<Button disabled={!enabled || busy} onClick={() => setMode("delete")}>
						Delete user
					</Button>
				</SpaceBetween>
				<DataTable
					title="User groups"
					poll={groups}
					columns={[
						{ id: "name", header: "Group", cell: (g) => g.GroupName },
						{
							id: "remove",
							header: "Membership",
							cell: (g) => (
								<Button
									disabled={!enabled || busy}
									onClick={() =>
										run(() =>
											cognitoCall(scope, "AdminRemoveUserFromGroup", {
												...input,
												GroupName: g.GroupName,
											}),
										)
									}
								>
									Remove
								</Button>
							),
						},
					]}
				/>
				{user.data && (
					<UserMessages
						key={JSON.stringify(user.data.UserAttributes)}
						scope={scope}
						attributes={user.data.UserAttributes ?? []}
					/>
				)}
				<Modal
					visible={!!mode}
					onDismiss={close}
					header={
						mode === "delete"
							? `Delete user ${username}?`
							: `${mode === "password" ? "Set password" : mode === "attributes" ? "Edit attributes" : "Add to group"}: ${username}`
					}
					footer={
						<SpaceBetween direction="horizontal" size="s">
							<Button disabled={busy} onClick={close}>
								Cancel
							</Button>
							<Button
								variant="primary"
								loading={busy}
								disabled={!enabled}
								onClick={() =>
									run(async () => {
										if (mode === "delete")
											return cognitoCall(scope, "AdminDeleteUser", input);
										if (mode === "password") {
											if (!text) throw new Error("Enter a password.");
											return cognitoCall(scope, "AdminSetUserPassword", {
												...input,
												Password: text,
												Permanent: permanent,
											});
										}
										if (mode === "group") {
											if (!text.trim()) throw new Error("Enter a group name.");
											return cognitoCall(scope, "AdminAddUserToGroup", {
												...input,
												GroupName: text.trim(),
											});
										}
										const values = stringMap(text, "Attributes");
										const before = user.data?.UserAttributes ?? [];
										const removed = removedAttributes(before, values);
										if (removed.length)
											await cognitoCall(scope, "AdminDeleteUserAttributes", {
												...input,
												UserAttributeNames: removed,
											});
										return cognitoCall(scope, "AdminUpdateUserAttributes", {
											...input,
											UserAttributes: changedAttributes(before, values),
										});
									}, mode === "delete")
								}
							>
								{mode === "delete" ? "Delete user" : "Save"}
							</Button>
						</SpaceBetween>
					}
				>
					<SpaceBetween size="m">
						{!!error && <Alert type="error">{error}</Alert>}
						{mode === "delete" ? (
							<div>Permanently delete {username} from this pool?</div>
						) : mode === "attributes" ? (
							<FormField
								label="Attributes (JSON object of string values; sub is read-only)"
								description="Omit a key to remove that attribute."
							>
								<Textarea
									value={text}
									onChange={({ detail }) => setText(detail.value)}
								/>
							</FormField>
						) : (
							<Field
								label={mode === "password" ? "Password" : "Group name"}
								value={text}
								onChange={setText}
								password={mode === "password"}
							/>
						)}
						{mode === "password" && (
							<Checkbox
								checked={permanent}
								onChange={({ detail }) => setPermanent(detail.checked)}
							>
								Permanent password
							</Checkbox>
						)}
					</SpaceBetween>
				</Modal>
			</SpaceBetween>
		</Container>
	);
}
function UserMessages({
	scope,
	attributes,
}: {
	scope: CognitoScope;
	attributes: { Name: string; Value: string }[];
}) {
	const messages = usePoll(async () => {
		const destinations = attributes
			.filter((a) => ["email", "phone_number"].includes(a.Name))
			.map((a) => a.Value);
		const lists = await Promise.all(
			destinations.map((to) =>
				mailbox.list({ channel: "", service: "cognito-idp", to, since: "" }),
			),
		);
		return [
			...new Map(
				lists
					.flat()
					.filter(
						(m) =>
							m.account === scope.account &&
							m.region === scope.region &&
							destinations.includes(m.to),
					)
					.map((m) => [m.id, m]),
			).values(),
		].sort((a, b) => b.time.localeCompare(a.time));
	});
	return (
		<DataTable
			title="Recent codes"
			poll={messages}
			columns={[
				{ id: "time", header: "Time", cell: (m) => m.time },
				{ id: "to", header: "Destination", cell: (m) => m.to },
				{
					id: "body",
					header: "Message",
					cell: (m) => <CodeBlock text={m.body ?? m.text ?? ""} />,
				},
			]}
		/>
	);
}
