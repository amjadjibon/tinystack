import {
	Alert,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	Input,
	Link,
	Modal,
	SpaceBetween,
	Table,
	type TableProps,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import {
	type Client,
	type CognitoScope,
	cognitoCall,
	cognitoPages,
	type Group,
	type Pool,
} from "#/lib/cognito";
import {
	type CognitoUser,
	changedAttributes,
	matchesUser,
} from "#/lib/cognitoHelpers";
import { errorText, stringMap } from "#/lib/consoleActions";
import { mailbox } from "#/lib/mailbox";
import { type Poll, usePoll } from "#/lib/usePoll";

function DataTable<T>({
	title,
	poll,
	columns,
	actions,
}: {
	title: string;
	poll: Poll<T[]>;
	columns: TableProps.ColumnDefinition<T>[];
	actions?: React.ReactNode;
}) {
	return (
		<SpaceBetween size="s">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				items={poll.data ?? []}
				loading={poll.loading}
				loadingText={`Loading ${title}`}
				empty={`No ${title.toLowerCase()}.`}
				columnDefinitions={columns}
				header={
					<Header
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								{actions}
								<Button onClick={poll.reload}>Refresh</Button>
							</SpaceBetween>
						}
					>
						{title}
					</Header>
				}
			/>
		</SpaceBetween>
	);
}
function Field({
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
			{selected && (
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
					{error && <Alert type="error">{error}</Alert>}
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
				{(user.error || error) && (
					<Alert type="error">{user.error || error}</Alert>
				)}
				{user.loading ? (
					"Loading user"
				) : (
					<>
						<div>
							Status: {user.data?.UserStatus ?? "—"} ·{" "}
							{user.data?.Enabled ? "Enabled" : "Disabled"}
						</div>
						<CodeBlock
							text={JSON.stringify(user.data?.UserAttributes ?? [], null, 2)}
						/>
					</>
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
										if (
											before.some(
												(a) => a.Name !== "sub" && !(a.Name in values),
											)
										)
											throw new Error(
												"Attribute removal is not supported by this editor. Retain existing keys.",
											);
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
						{error && <Alert type="error">{error}</Alert>}
						{mode === "delete" ? (
							<div>Permanently delete {username} from this pool?</div>
						) : mode === "attributes" ? (
							<FormField label="Attributes (JSON object of string values; sub is read-only)">
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
						),
					},
				]}
			/>
			{selected && (
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
						<>
							<Field label="Group name" value={name} onChange={setName} />
							<Field
								label="Description"
								value={description}
								onChange={setDescription}
							/>
						</>
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
export function CognitoClients({
	scope,
	enabled,
}: {
	scope: CognitoScope;
	enabled: boolean;
}) {
	const clients = usePoll(async () => {
		if (!enabled) return [];
		const list = await cognitoPages<Client>(
			scope,
			"ListUserPoolClients",
			"UserPoolClients",
		);
		return Promise.all(
			list.map(async (c) => {
				const { ClientSecret: _secret, ...metadata } = (
					await cognitoCall(scope, "DescribeUserPoolClient", {
						ClientId: c.ClientId,
					})
				).UserPoolClient;
				return metadata;
			}),
		);
	});
	return (
		<DataTable
			title="App clients"
			poll={clients}
			columns={[
				{ id: "name", header: "Name", cell: (c) => c.ClientName },
				{ id: "id", header: "Client ID", cell: (c) => c.ClientId },
				{
					id: "secret",
					header: "Client secret",
					cell: (c) => (
						<ClientSecret
							key={c.ClientId}
							scope={scope}
							client={c.ClientId}
							enabled={enabled}
						/>
					),
				},
				{
					id: "callback",
					header: "Callback URLs",
					cell: (c) => (c.CallbackURLs ?? []).join(", ") || "—",
				},
				{
					id: "logout",
					header: "Logout URLs",
					cell: (c) => (c.LogoutURLs ?? []).join(", ") || "—",
				},
				{
					id: "oauth",
					header: "OAuth flows",
					cell: (c) => (c.AllowedOAuthFlows ?? []).join(", ") || "—",
				},
				{
					id: "auth",
					header: "Auth flows",
					cell: (c) => (c.ExplicitAuthFlows ?? []).join(", ") || "—",
				},
			]}
		/>
	);
}
function ClientSecret({
	scope,
	client,
	enabled,
}: {
	scope: CognitoScope;
	client: string;
	enabled: boolean;
}) {
	const [secret, setSecret] = useState<string>(),
		[busy, setBusy] = useState(false),
		[error, setError] = useState("");
	return (
		<SpaceBetween size="xs">
			{error && <Alert type="error">{error}</Alert>}
			{secret !== undefined ? (
				<>
					<CodeBlock text={secret || "No client secret"} />
					<Button onClick={() => setSecret(undefined)}>Hide</Button>
				</>
			) : (
				<>
					<span>••••••••</span>
					<Button
						disabled={!enabled}
						loading={busy}
						onClick={async () => {
							setBusy(true);
							setError("");
							try {
								setSecret(
									(
										await cognitoCall(scope, "DescribeUserPoolClient", {
											ClientId: client,
										})
									).UserPoolClient.ClientSecret ?? "",
								);
							} catch (e) {
								setError(errorText(e));
							} finally {
								setBusy(false);
							}
						}}
					>
						Reveal
					</Button>
				</>
			)}
		</SpaceBetween>
	);
}
export function CognitoDomain({
	scope,
	pool,
	enabled,
}: {
	scope: CognitoScope;
	pool: Pool;
	enabled: boolean;
}) {
	const domain = pool.CustomDomain || pool.Domain;
	const info = usePoll(() =>
		enabled && domain
			? cognitoCall(scope, "DescribeUserPoolDomain", { Domain: domain })
			: Promise.resolve(undefined),
	);
	const host = domain?.includes(".")
		? domain
		: `${domain}.auth.${scope.region}.amazoncognito.com`;
	return (
		<SpaceBetween size="m">
			{info.error && <Alert type="error">{info.error}</Alert>}
			{!domain ? (
				<div>No domain configured.</div>
			) : (
				<>
					<div>Domain: {domain}</div>
					<div>
						Status:{" "}
						{info.loading
							? "Loading"
							: (info.data?.DomainDescription?.Status ?? "—")}
					</div>
					<Link external href={`https://${host}/login`}>
						Hosted UI
					</Link>
					<Alert type="info">
						Use your local gateway with this domain's Host routing and
						configured TLS. Sign-in requires client_id, response_type and a
						registered redirect_uri.
					</Alert>
				</>
			)}
		</SpaceBetween>
	);
}
