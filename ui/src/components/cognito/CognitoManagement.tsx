import {
	Alert,
	Button,
	Checkbox,
	FormField,
	Input,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type CognitoScope,
	cognitoCall,
	cognitoHref,
	type Pool,
} from "#/lib/cognito";
import {
	type CognitoField,
	editableSettings,
	fieldValue,
	formInput,
	poolUpdateKeys,
} from "#/lib/cognitoHelpers";
import { usePoll } from "#/lib/usePoll";

export const poolFields: CognitoField[] = [
	{
		key: "DeletionProtection",
		label: "Deletion protection",
		options: ["ACTIVE", "INACTIVE"],
	},
	{
		key: "MfaConfiguration",
		label: "MFA configuration (configure a delivery method before enabling)",
		options: ["OFF", "OPTIONAL", "ON"],
	},
	{
		key: "SmsConfiguration",
		label: "SMS configuration (JSON; SnsCallerArn and ExternalId)",
		kind: "json",
	},
	{ key: "PoolName", label: "Pool name", required: true },
	{
		key: "Policies.PasswordPolicy.MinimumLength",
		label: "Minimum password length",
		kind: "number",
		min: 6,
		max: 99,
	},
	...[
		"RequireUppercase",
		"RequireLowercase",
		"RequireNumbers",
		"RequireSymbols",
	].map((key) => ({
		key: `Policies.PasswordPolicy.${key}`,
		label: key.replace(/([A-Z])/g, " $1").trim(),
		kind: "boolean" as const,
	})),
	{
		key: "Policies.PasswordPolicy.TemporaryPasswordValidityDays",
		label: "Temporary password validity (days)",
		kind: "number",
		max: 365,
	},
	{
		key: "AutoVerifiedAttributes",
		label: "Auto-verified attributes (email, phone_number)",
		kind: "list",
	},
];
export const clientFields: CognitoField[] = [
	{ key: "ClientName", label: "Client name", required: true },
	{
		key: "ExplicitAuthFlows",
		label: "Auth flows (comma separated ALLOW_* names)",
		kind: "list",
	},
	{
		key: "AllowedOAuthFlowsUserPoolClient",
		label: "Enable OAuth flows",
		kind: "boolean",
	},
	{
		key: "AllowedOAuthFlows",
		label: "OAuth flows (code, implicit, client_credentials)",
		kind: "list",
	},
	{
		key: "AllowedOAuthScopes",
		label: "OAuth scopes (comma separated)",
		kind: "list",
	},
	{
		key: "SupportedIdentityProviders",
		label: "Identity providers (COGNITO for local sign-in)",
		kind: "list",
	},
	{ key: "CallbackURLs", label: "Callback URLs (one per line)", kind: "list" },
	{ key: "LogoutURLs", label: "Logout URLs (one per line)", kind: "list" },
	...["AccessToken", "IdToken", "RefreshToken"].flatMap((token) => [
		{
			key: `${token}Validity`,
			label: `${token} validity`,
			kind: "number" as const,
			min: 1,
		},
		{
			key: `TokenValidityUnits.${token}`,
			label: `${token} validity unit`,
			options: ["seconds", "minutes", "hours", "days"],
		},
	]),
];
export function CognitoForm({
	title,
	fields,
	initial = {},
	confirmation,
	onClose,
	onSaved,
	save,
}: {
	title: string;
	fields: CognitoField[];
	initial?: Record<string, unknown>;
	confirmation?: string;
	onClose: () => void;
	onSaved: () => void;
	save: (input: Record<string, unknown>) => Promise<unknown>;
}) {
	const [values, setValues] = useState<Record<string, string>>(() =>
		Object.fromEntries(
			fields.map((f) => {
				const v = fieldValue(initial, f.key);
				return [
					f.key,
					v === undefined
						? ""
						: f.kind === "list"
							? (v as string[]).join("\n")
							: f.kind === "json"
								? JSON.stringify(v, null, 2)
								: String(v),
				];
			}),
		),
	);
	return (
		<ActionDialog
			title={title}
			confirmation={confirmation}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = formInput(initial, fields, values);
				return [{ label: title, run: () => save(input) }];
			}}
		>
			<SpaceBetween size="m">
				{confirmation && (
					<Alert type="warning">Permanently delete {confirmation}?</Alert>
				)}
				{fields.map((f) => {
					const value = values[f.key] ?? "";
					const change = (v: string) => setValues({ ...values, [f.key]: v });
					return (
						<FormField key={f.key} label={f.label}>
							{f.kind === "boolean" ? (
								<Checkbox
									checked={value === "true"}
									onChange={({ detail }) => change(String(detail.checked))}
								>
									{f.label}
								</Checkbox>
							) : f.options ? (
								<Select
									ariaLabel={f.label}
									options={f.options.map((v) => ({ label: v, value: v }))}
									selectedOption={value ? { label: value, value } : null}
									onChange={({ detail }) =>
										change(detail.selectedOption.value ?? "")
									}
								/>
							) : f.kind === "list" || f.kind === "json" ? (
								<Textarea
									ariaLabel={f.label}
									value={value}
									onChange={({ detail }) => change(detail.value)}
								/>
							) : (
								<Input
									ariaLabel={f.label}
									value={value}
									type={f.kind === "number" ? "number" : "text"}
									onChange={({ detail }) => change(detail.value)}
								/>
							)}
						</FormField>
					);
				})}
			</SpaceBetween>
		</ActionDialog>
	);
}
export function PoolActions({
	scope,
	pool,
	enabled,
	onChanged,
}: {
	scope: CognitoScope;
	pool?: Pool;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [mode, setMode] = useState("");
	return (
		<SpaceBetween direction="horizontal" size="s">
			<Button
				disabled={!enabled || (!!scope.id && !pool)}
				onClick={() => setMode(scope.id ? "edit" : "create")}
			>
				{scope.id ? "Edit pool" : "Create user pool"}
			</Button>
			{scope.id && (
				<Button disabled={!enabled || !pool} onClick={() => setMode("delete")}>
					Delete pool
				</Button>
			)}
			{mode && (
				<CognitoForm
					title={
						mode === "create"
							? "Create user pool"
							: mode === "edit"
								? "Edit pool"
								: "Delete pool"
					}
					confirmation={mode === "delete" ? pool?.Name : undefined}
					fields={
						mode === "delete"
							? []
							: [
									...poolFields,
									...(mode === "create"
										? [
												{
													key: "UsernameAttributes",
													label: "Username attributes (email, phone_number)",
													kind: "list" as const,
												},
												{
													key: "AliasAttributes",
													label:
														"Alias attributes (email, phone_number, preferred_username)",
													kind: "list" as const,
												},
											]
										: []),
								]
					}
					initial={
						mode === "edit" && pool
							? {
									...editableSettings(pool, poolUpdateKeys),
									PoolName: pool.Name,
								}
							: mode === "create"
								? {
										Policies: {
											PasswordPolicy: {
												MinimumLength: 8,
												RequireUppercase: true,
												RequireLowercase: true,
												RequireNumbers: true,
												RequireSymbols: true,
												TemporaryPasswordValidityDays: 7,
											},
										},
									}
								: {}
					}
					onClose={() => setMode("")}
					onSaved={() => {
						setMode("");
						if (mode === "delete")
							window.location.assign(cognitoHref({ ...scope, id: undefined }));
						else onChanged();
					}}
					save={(input) =>
						mode === "delete"
							? cognitoCall(scope, "DeleteUserPool", {})
							: cognitoCall(
									scope,
									mode === "create" ? "CreateUserPool" : "UpdateUserPool",
									input,
								)
					}
				/>
			)}
		</SpaceBetween>
	);
}
export function PoolSettings({
	scope,
	enabled,
}: {
	scope: CognitoScope;
	enabled: boolean;
}) {
	const arn = `arn:aws:cognito-idp:${scope.region}:${scope.account}:userpool/${scope.id}`;
	const mfa = usePoll(() =>
		enabled
			? cognitoCall(scope, "GetUserPoolMfaConfig", {})
			: Promise.resolve(undefined),
	);
	const tags = usePoll(() =>
		enabled
			? cognitoCall(scope, "ListTagsForResource", { ResourceArn: arn })
			: Promise.resolve(undefined),
	);
	const [mode, setMode] = useState("");
	return (
		<SpaceBetween size="l">
			{mfa.error && <Alert type="error">{mfa.error}</Alert>}
			<div>MFA: {String(mfa.data?.MfaConfiguration ?? "OFF")}</div>
			<SpaceBetween direction="horizontal" size="s">
				<Button disabled={!enabled || !mfa.data} onClick={() => setMode("mfa")}>
					Configure MFA
				</Button>
				<Button disabled={!enabled} onClick={() => setMode("attribute")}>
					Add custom attribute
				</Button>
			</SpaceBetween>
			{tags.error && <Alert type="error">{tags.error}</Alert>}
			<ConsoleTags
				tags={tags.data?.Tags ?? {}}
				enabled={enabled && !!tags.data}
				onChanged={tags.reload}
				plan={(next, removed) => [
					...(removed.length
						? [
								{
									label: "Remove tags",
									run: () =>
										cognitoCall(scope, "UntagResource", {
											ResourceArn: arn,
											TagKeys: removed,
										}),
								},
							]
						: []),
					...(Object.keys(next).length
						? [
								{
									label: "Set tags",
									run: () =>
										cognitoCall(scope, "TagResource", {
											ResourceArn: arn,
											Tags: next,
										}),
								},
							]
						: []),
				]}
			/>
			{mode && (
				<CognitoForm
					title={mode === "mfa" ? "Configure MFA" : "Add custom attribute"}
					initial={
						mode === "mfa"
							? mfa.data
							: { AttributeDataType: "String", Mutable: true }
					}
					fields={
						mode === "mfa"
							? [
									{
										key: "MfaConfiguration",
										label: "MFA configuration",
										options: ["OFF", "OPTIONAL", "ON"],
										required: true,
									},
									{
										key: "SoftwareTokenMfaConfiguration.Enabled",
										label: "Enable software token MFA",
										kind: "boolean",
									},
									{
										key: "SmsMfaConfiguration",
										label: "SMS MFA configuration (JSON; optional)",
										kind: "json",
									},
								]
							: [
									{
										key: "Name",
										label: "Attribute name (without custom: prefix)",
										required: true,
									},
									{
										key: "AttributeDataType",
										label: "Data type",
										options: ["String", "Number", "Boolean", "DateTime"],
										required: true,
									},
									{ key: "Mutable", label: "Mutable", kind: "boolean" },
								]
					}
					onClose={() => setMode("")}
					onSaved={() => {
						setMode("");
						mfa.reload();
					}}
					save={(input) =>
						mode === "mfa"
							? cognitoCall(scope, "SetUserPoolMfaConfig", input)
							: cognitoCall(scope, "AddCustomAttributes", {
									CustomAttributes: [input],
								})
					}
				/>
			)}
		</SpaceBetween>
	);
}
