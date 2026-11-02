import {
	Alert,
	Checkbox,
	FormField,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice, Field } from "#/components/containers/Controls";
import { type AppSyncScope, appsyncCall } from "#/lib/appsync";
import {
	fields,
	formInput,
	initialForm,
	type RecordValue,
	type ResourceKind,
	resourceIdentity,
	resourceLabel,
} from "#/lib/appsyncHelpers";

const labels = {
	api: "GraphQL API",
	source: "data source",
	resolver: "resolver",
	function: "function",
	key: "API key",
};
const operations = {
	api: "GraphqlApi",
	source: "DataSource",
	resolver: "Resolver",
	function: "Function",
	key: "ApiKey",
};
export function ResourceDialog({
	scope,
	apiId,
	kind,
	row,
	typeName,
	remove = false,
	onClose,
	onSaved,
}: {
	scope: AppSyncScope;
	apiId?: string;
	kind: ResourceKind;
	typeName?: string;
	row?: RecordValue;
	remove?: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(() =>
		initialForm(kind, row ?? (typeName ? { TypeName: typeName } : {})),
	);
	const operation =
		(remove ? "Delete" : row ? "Update" : "Create") + operations[kind];
	const immutable = row ? Object.keys(resourceIdentity(kind, row)) : [];
	return (
		<ActionDialog
			title={`${remove ? "Delete" : row ? "Edit" : "Create"} ${labels[kind]}`}
			submit={remove ? "Delete" : row ? "Save changes" : "Create"}
			confirmation={remove && row ? resourceLabel(kind, row) : undefined}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = {
					...(apiId ? { ApiId: apiId } : {}),
					...(!remove ? formInput(kind, form) : {}),
					...(row ? resourceIdentity(kind, row) : {}),
				};
				return [
					{ label: operation, run: () => appsyncCall(scope, operation, input) },
				];
			}}
		>
			{remove ? (
				<Alert type="warning">
					Permanently delete {row && resourceLabel(kind, row)}. Deleting an API
					also deletes its schema, data sources, resolvers, functions and API
					keys.
				</Alert>
			) : (
				<SpaceBetween size="m">
					{fields[kind].map((f) =>
						immutable.includes(f.key) ? (
							<FormField key={f.key} label={f.label}>
								{form[f.key]}
							</FormField>
						) : f.kind === "boolean" ? (
							<Checkbox
								key={f.key}
								checked={form[f.key] === "true"}
								onChange={({ detail }) =>
									setForm({ ...form, [f.key]: String(detail.checked) })
								}
							>
								{f.label}
							</Checkbox>
						) : f.choices ? (
							<Choice
								key={f.key}
								label={f.label}
								value={form[f.key]}
								options={f.choices}
								onChange={(v) => setForm({ ...form, [f.key]: v })}
							/>
						) : (
							<FormField
								key={f.key}
								description={
									f.schema
										? `Supported members: ${configMembers(f.schema)}. ${f.example ?? ""}`
										: undefined
								}
							>
								<Field
									label={f.label + (f.required ? " (required)" : "")}
									value={form[f.key]}
									multiline={f.kind === "text" || f.kind === "json"}
									onChange={(v) => setForm({ ...form, [f.key]: v })}
								/>
							</FormField>
						),
					)}
				</SpaceBetween>
			)}
		</ActionDialog>
	);
}
function configMembers(schema: string) {
	const members: Record<string, string> = {
		LogConfig: "FieldLogLevel, CloudWatchLogsRoleArn, ExcludeVerboseContent",
		UserPoolConfig: "UserPoolId, AwsRegion, DefaultAction, AppIdClientRegex",
		OpenIDConnectConfig: "Issuer, ClientId, IatTTL, AuthTTL",
		LambdaAuthorizerConfig:
			"AuthorizerUri, AuthorizerResultTtlInSeconds, IdentityValidationExpression",
		AdditionalAuthenticationProvider:
			"AuthenticationType, UserPoolConfig (without DefaultAction), OpenIDConnectConfig, LambdaAuthorizerConfig",
		AppSyncRuntime: "Name, RuntimeVersion",
		PipelineConfig: "Functions (ordered function IDs)",
		LambdaDataSourceConfig: "LambdaFunctionArn",
		DynamodbDataSourceConfig: "TableName, AwsRegion, UseCallerCredentials",
		HttpDataSourceConfig:
			"Endpoint, AuthorizationConfig (AuthorizationType, AwsIamConfig: SigningRegion, SigningServiceName)",
		EventBridgeDataSourceConfig: "EventBusArn",
	};
	return members[schema] ?? "";
}
