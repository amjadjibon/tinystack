import {
	Alert,
	Button,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import {
	type Client,
	type CognitoScope,
	cognitoCall,
	cognitoPages,
	type Pool,
} from "#/lib/cognito";
import { clientUpdateKeys, editableSettings } from "#/lib/cognitoHelpers";
import { errorText } from "#/lib/consoleActions";
import { usePoll } from "#/lib/usePoll";
import { CognitoForm, clientFields } from "./CognitoManagement";
import { DataTable } from "./CognitoTable";
export function CognitoClients({
	scope,
	enabled,
}: {
	scope: CognitoScope;
	enabled: boolean;
}) {
	const [mode, setMode] = useState("");
	const [selected, setSelected] = useState<Client>();
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
		<SpaceBetween size="l">
			<DataTable
				actions={
					<Button
						disabled={!enabled}
						onClick={() => {
							setSelected(undefined);
							setMode("create");
						}}
					>
						Create app client
					</Button>
				}
				title="App clients"
				poll={clients}
				columns={[
					{ id: "name", header: "Name", cell: (c) => c.ClientName },
					{ id: "id", header: "Client ID", cell: (c) => c.ClientId },
					{
						id: "actions",
						header: "Actions",
						cell: (c) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									disabled={!enabled}
									onClick={() => {
										setSelected(c);
										setMode("edit");
									}}
								>
									Edit
								</Button>
								<Button
									disabled={!enabled}
									onClick={() => {
										setSelected(c);
										setMode("secret");
									}}
								>
									Secret
								</Button>
								<Button
									disabled={!enabled}
									onClick={() => {
										setSelected(c);
										setMode("delete");
									}}
								>
									Delete
								</Button>
							</SpaceBetween>
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
			{mode === "secret" && selected && (
				<SpaceBetween size="s">
					<div>Secret for {selected.ClientName}</div>
					<ClientSecret
						key={selected.ClientId}
						scope={scope}
						client={selected.ClientId}
						enabled={enabled}
					/>
					<Button onClick={() => setMode("")}>Close secret</Button>
				</SpaceBetween>
			)}
			{!!mode && mode !== "secret" && (
				<CognitoForm
					title={`${mode === "create" ? "Create" : mode === "edit" ? "Edit" : "Delete"} app client`}
					confirmation={mode === "delete" ? selected?.ClientName : undefined}
					initial={
						selected
							? editableSettings(selected, clientUpdateKeys)
							: {
									SupportedIdentityProviders: ["COGNITO"],
									AccessTokenValidity: 1,
									IdTokenValidity: 1,
									RefreshTokenValidity: 30,
									TokenValidityUnits: {
										AccessToken: "hours",
										IdToken: "hours",
										RefreshToken: "days",
									},
								}
					}
					fields={
						mode === "delete"
							? []
							: [
									...clientFields,
									...(mode === "create"
										? [
												{
													key: "GenerateSecret",
													label: "Generate client secret",
													kind: "boolean" as const,
												},
											]
										: []),
								]
					}
					onClose={() => setMode("")}
					onSaved={() => {
						setMode("");
						setSelected(undefined);
						clients.reload();
					}}
					save={(input) =>
						mode === "delete"
							? cognitoCall(scope, "DeleteUserPoolClient", {
									ClientId: selected?.ClientId ?? "",
								})
							: cognitoCall(
									scope,
									mode === "create"
										? "CreateUserPoolClient"
										: "UpdateUserPoolClient",
									input,
								)
					}
				/>
			)}
		</SpaceBetween>
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
				<SpaceBetween size="xs">
					<CodeBlock text={secret || "No client secret"} />
					<Button onClick={() => setSecret(undefined)}>Hide</Button>
				</SpaceBetween>
			) : (
				<SpaceBetween size="xs">
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
				</SpaceBetween>
			)}
		</SpaceBetween>
	);
}
export function CognitoDomain({
	scope,
	pool,
	enabled,
	onChanged,
}: {
	scope: CognitoScope;
	pool: Pool;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [mode, setMode] = useState("");
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
			<SpaceBetween direction="horizontal" size="s">
				<Button
					disabled={!enabled || (!!domain && !info.data)}
					onClick={() => setMode(domain ? "edit" : "create")}
				>
					{domain ? "Edit domain" : "Create domain"}
				</Button>
				{domain && (
					<Button disabled={!enabled} onClick={() => setMode("delete")}>
						Delete domain
					</Button>
				)}
			</SpaceBetween>
			{!!mode && (
				<CognitoForm
					title={`${mode === "create" ? "Create" : mode === "edit" ? "Edit" : "Delete"} domain`}
					confirmation={mode === "delete" ? domain : undefined}
					initial={
						mode === "create"
							? { ManagedLoginVersion: 1 }
							: { ...info.data?.DomainDescription, Domain: domain }
					}
					fields={
						mode === "delete"
							? []
							: [
									...(mode === "create"
										? [
												{
													key: "Domain",
													label: "Domain prefix or custom hostname",
													required: true,
												},
											]
										: []),
									{
										key: "ManagedLoginVersion",
										label: "Managed login version",
										kind: "number",
										min: 1,
										max: 2,
									},
									{
										key: "CustomDomainConfig",
										label:
											"Custom domain configuration (JSON; CertificateArn required for custom domains)",
										kind: "json",
									},
								]
					}
					onClose={() => setMode("")}
					onSaved={() => {
						setMode("");
						info.reload();
						onChanged();
					}}
					save={(input) => {
						const config = input.CustomDomainConfig as
							| Record<string, unknown>
							| undefined;
						const payload = {
							Domain: input.Domain,
							ManagedLoginVersion: input.ManagedLoginVersion,
							...(config && Object.keys(config).length
								? { CustomDomainConfig: config }
								: {}),
						};
						return mode === "delete"
							? cognitoCall(scope, "DeleteUserPoolDomain", {
									Domain: domain ?? "",
								})
							: cognitoCall(
									scope,
									mode === "create"
										? "CreateUserPoolDomain"
										: "UpdateUserPoolDomain",
									payload,
								);
					}}
				/>
			)}
			{info.error && <Alert type="error">{info.error}</Alert>}
			{!domain ? (
				<div>No domain configured.</div>
			) : (
				<SpaceBetween size="m">
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
				</SpaceBetween>
			)}
		</SpaceBetween>
	);
}
