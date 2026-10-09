import {
	Alert,
	Box,
	Button,
	Container,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import {
	apiCall,
	type GatewayAuthorizer,
	type GatewayScope,
	type GatewaySnapshot,
	isHTTP,
	listPatch,
	scalarPatch,
} from "#/lib/apigateway";
import { jsonObject } from "#/lib/consoleActions";
import { GatewayInput, GatewaySelect, GatewayText, options } from "./Fields";
import { GatewayTable } from "./GatewayTable";
export function Authorizers({
	scope,
	snapshot,
	enabled,
	onChanged,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	enabled: boolean;
	onChanged: (message: string) => void;
}) {
	const [selected, setSelected] = useState<GatewayAuthorizer>(),
		[dialog, setDialog] = useState("");
	const done = () => {
		setDialog("");
		onChanged("Authorizer changes saved.");
	};
	return (
		<SpaceBetween size="l">
			<GatewayTable
				items={snapshot.authorizers}
				trackBy={(a) => a.AuthorizerId ?? a.Id ?? ""}
				wrapLines
				header={
					<Header
						actions={
							<Button
								variant="primary"
								disabled={!enabled}
								onClick={() => {
									setSelected(undefined);
									setDialog("form");
								}}
							>
								Create authorizer
							</Button>
						}
					>
						Authorizers
					</Header>
				}
				columnDefinitions={[
					{ id: "name", header: "Name", cell: (a) => a.Name ?? "—" },
					{
						id: "type",
						header: "Type",
						cell: (a) => a.AuthorizerType ?? a.Type ?? "—",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (a) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									variant="inline-link"
									onClick={() => {
										setSelected(a);
										setDialog("details");
									}}
								>
									Details
								</Button>
								<Button
									variant="inline-link"
									disabled={!enabled}
									onClick={() => {
										setSelected(a);
										setDialog("form");
									}}
								>
									Edit
								</Button>
								<Button
									variant="inline-link"
									disabled={!enabled}
									onClick={() => {
										setSelected(a);
										setDialog("delete");
									}}
								>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
				empty={
					<Box>
						No authorizers. Create one, then attach it to a route or method.
					</Box>
				}
			/>
			{dialog === "details" && selected && (
				<Container
					header={
						<Header
							actions={<Button onClick={() => setDialog("")}>Close</Button>}
						>
							Authorizer configuration
						</Header>
					}
				>
					<CodeBlock text={JSON.stringify(selected, null, 2)} />
				</Container>
			)}
			{dialog === "form" && (
				<AuthorizerForm
					scope={scope}
					initial={selected}
					onClose={() => setDialog("")}
					onSaved={done}
				/>
			)}
			{dialog === "delete" && selected && (
				<ActionDialog
					title="Delete authorizer"
					submit="Delete authorizer"
					confirmation={selected.Name ?? selected.Id ?? selected.AuthorizerId}
					onClose={() => setDialog("")}
					onSaved={done}
					plan={async () => [
						{
							label: "Delete authorizer",
							run: () =>
								apiCall(scope, "DeleteAuthorizer", {
									AuthorizerId: selected.AuthorizerId ?? selected.Id,
								}),
						},
					]}
				>
					<Box>
						Detach this authorizer from routes or methods first. Existing
						deployments retain their snapshots.
					</Box>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function AuthorizerForm({
	scope,
	initial,
	onClose,
	onSaved,
}: {
	scope: GatewayScope;
	initial?: GatewayAuthorizer;
	onClose: () => void;
	onSaved: () => void;
}) {
	const http = isHTTP(scope),
		[name, setName] = useState(initial?.Name ?? ""),
		[type, setType] = useState(
			initial?.AuthorizerType ?? initial?.Type ?? (http ? "JWT" : "TOKEN"),
		);
	const defaults = (t: string) =>
		http
			? t === "JWT"
				? {
						IdentitySource: ["$request.header.Authorization"],
						JwtConfiguration: {
							Issuer: "https://issuer.example.com",
							Audience: ["my-app"],
						},
					}
				: {
						IdentitySource: ["$request.header.Authorization"],
						AuthorizerUri: "",
						AuthorizerPayloadFormatVersion: "2.0",
						EnableSimpleResponses: true,
						AuthorizerResultTtlInSeconds: 0,
					}
			: t === "COGNITO_USER_POOLS"
				? {
						IdentitySource: "method.request.header.Authorization",
						ProviderARNs: [],
						AuthorizerResultTtlInSeconds: 0,
					}
				: {
						IdentitySource: "method.request.header.Authorization",
						AuthorizerUri: "",
						AuthorizerResultTtlInSeconds: 0,
					};
	const settings = initial
		? Object.fromEntries(
				Object.entries(initial).filter(
					([k]) =>
						!["Name", "Type", "AuthorizerType", "Id", "AuthorizerId"].includes(
							k,
						),
				),
			)
		: defaults(type);
	const [text, setText] = useState(JSON.stringify(settings, null, 2));
	return (
		<ActionDialog
			title={initial ? "Edit authorizer" : "Create authorizer"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!name.trim()) throw new Error("Authorizer name is required.");
				const config = jsonObject(text, "Authorizer settings");
				const known = http
					? [
							"IdentitySource",
							"JwtConfiguration",
							"AuthorizerUri",
							"AuthorizerPayloadFormatVersion",
							"EnableSimpleResponses",
							"AuthorizerResultTtlInSeconds",
						]
					: [
							"IdentitySource",
							"AuthorizerUri",
							"AuthorizerCredentials",
							"IdentityValidationExpression",
							"ProviderARNs",
							"AuthType",
							"AuthorizerResultTtlInSeconds",
						];
				if (Object.keys(config).some((k) => !known.includes(k)))
					throw new Error(
						"Use the supported authorizer settings shown in the form.",
					);
				if (http)
					return [
						{
							label: "Save authorizer",
							run: () =>
								apiCall(
									scope,
									initial ? "UpdateAuthorizer" : "CreateAuthorizer",
									{
										Name: name,
										AuthorizerType: type,
										...config,
										...(initial ? { AuthorizerId: initial.AuthorizerId } : {}),
									},
								),
						},
					];
				if (!initial)
					return [
						{
							label: "Create authorizer",
							run: () =>
								apiCall(scope, "CreateAuthorizer", {
									Name: name,
									Type: type,
									...config,
								}),
						},
					];
				const patches = scalarPatch({ name, type });
				for (const [k, v] of Object.entries(config)) {
					const wire = k[0].toLowerCase() + k.slice(1);
					if (k === "ProviderARNs") {
						if (!Array.isArray(v) || v.some((p) => typeof p !== "string"))
							throw new Error("ProviderARNs must be an array of strings.");
						patches.push(
							...listPatch(
								"providerARNs",
								v,
								(initial.ProviderARNs as string[]) ?? [],
							),
						);
					} else if (v !== null && typeof v === "object")
						throw new Error(
							"REST authorizer settings must use scalar values or ProviderARNs.",
						);
					else patches.push(...scalarPatch({ [wire]: v }));
				}
				return [
					{
						label: "Update authorizer",
						run: () =>
							apiCall(scope, "UpdateAuthorizer", {
								AuthorizerId: initial.Id,
								PatchOperations: patches,
							}),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<GatewayInput label="Authorizer name" value={name} onChange={setName} />
				<GatewaySelect
					label="Authorizer type"
					value={type}
					onChange={(v) => {
						setType(v);
						setText(JSON.stringify(defaults(v), null, 2));
					}}
					options={options(
						initial
							? [type]
							: http
								? ["JWT", "REQUEST"]
								: ["TOKEN", "REQUEST", "COGNITO_USER_POOLS"],
					)}
				/>
				<GatewayText
					label="Authorizer settings (JSON)"
					value={text}
					onChange={setText}
					rows={12}
					description={
						http
							? "JWT: JwtConfiguration with Issuer and Audience. REQUEST: Lambda AuthorizerUri, IdentitySource, payload format and simple responses."
							: "TOKEN/REQUEST: Lambda AuthorizerUri and IdentitySource. Cognito: ProviderARNs and IdentitySource."
					}
				/>
				<Alert type="info">
					Settings use SDK field names. Authorizer caching is not enforced.
					Changes require deployment unless the HTTP stage auto-deploys.
				</Alert>
			</SpaceBetween>
		</ActionDialog>
	);
}
