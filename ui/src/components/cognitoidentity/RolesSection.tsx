import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	Container,
	ExpandableSection,
	FormField,
	Header,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { Text } from "#/components/memorydb/Fields";
import { type IdentityScope, identityCall } from "#/lib/cognitoidentity";
import {
	formFromRoles,
	type IdentityPool,
	mappingKeys,
	type PoolRoles,
	type RolesForm,
	rolesInput,
	ruleText,
} from "#/lib/cognitoidentityHelpers";
import { Copy, Field } from "./Field";

const example = JSON.stringify(
	{
		"cognito-idp.us-east-1.amazonaws.com/us-east-1_AbCdEfGhI:app-client-id": {
			Type: "Rules",
			AmbiguousRoleResolution: "AuthenticatedRole",
			RulesConfiguration: {
				Rules: [
					{
						Claim: "cognito:groups",
						MatchType: "Contains",
						Value: "admins",
						RoleARN: "arn:aws:iam::000000000000:role/admins",
					},
				],
			},
		},
	},
	null,
	2,
);

function RolesDialog({
	scope,
	pool,
	roles,
	onClose,
	onSaved,
}: {
	scope: IdentityScope;
	pool: IdentityPool;
	roles: PoolRoles;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState<RolesForm>(() => formFromRoles(roles));
	const set = (key: keyof RolesForm, value: string) =>
		setForm((f) => ({ ...f, [key]: value }));
	const keys = mappingKeys(pool);
	return (
		<ActionDialog
			title="Edit roles"
			submit="Save roles"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = rolesInput(pool.IdentityPoolId, form);
				return [
					{
						label: "SetIdentityPoolRoles",
						run: () => identityCall(scope, "SetIdentityPoolRoles", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					Saving replaces the pool's roles and role mappings. A role's trust
					policy must allow cognito-identity.amazonaws.com to assume it with
					AssumeRoleWithWebIdentity.
				</Alert>
				<Text
					label="Authenticated role ARN"
					value={form.authenticated}
					onChange={(v) => set("authenticated", v)}
					description="The role of identities that signed in with a provider."
				/>
				<Text
					label="Unauthenticated role ARN"
					value={form.unauthenticated}
					onChange={(v) => set("unauthenticated", v)}
					description="The role of identities without a login. Needed for the test below."
				/>
				<FormField
					label="Role mappings (JSON)"
					description={`An object of mappings by provider key; use {} for none. Each mapping needs a Type (Token or Rules) and an AmbiguousRoleResolution (AuthenticatedRole or Deny), and Rules mappings list 1 to 25 rules with a Claim, MatchType (Equals, NotEqual, StartsWith or Contains), Value and RoleARN. ${
						keys.length
							? `Keys for this pool's providers: ${keys.join(", ")}.`
							: "This pool has no user pool or OpenID Connect providers to map."
					}`}
				>
					<Textarea
						ariaLabel="Role mappings"
						rows={10}
						value={form.mappings}
						onChange={({ detail }) => set("mappings", detail.value)}
					/>
				</FormField>
				<ExpandableSection headerText="Example role mapping">
					<CodeBlock text={example} />
				</ExpandableSection>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function RolesSection({
	scope,
	pool,
	roles,
	enabled,
	onChanged,
}: {
	scope: IdentityScope;
	pool: IdentityPool;
	roles: PoolRoles;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [editing, setEditing] = useState(false);
	const mappings = Object.entries(roles.RoleMappings ?? {});
	return (
		<>
			<Container
				header={
					<Header
						variant="h2"
						actions={
							<Button disabled={!enabled} onClick={() => setEditing(true)}>
								Edit roles
							</Button>
						}
					>
						Roles
					</Header>
				}
			>
				<SpaceBetween size="m">
					<ColumnLayout columns={2} variant="text-grid">
						{(
							[
								["authenticated", "Authenticated role"],
								["unauthenticated", "Unauthenticated role"],
							] as const
						).map(([kind, label]) => (
							<Field key={kind} label={label}>
								{roles.Roles?.[kind] ? (
									<Copy text={roles.Roles[kind]} label={`${label} ARN`} />
								) : (
									"-"
								)}
							</Field>
						))}
					</ColumnLayout>
					<Table
						variant="embedded"
						header={
							<Header variant="h3" counter={`(${mappings.length})`}>
								Role mappings
							</Header>
						}
						items={mappings}
						trackBy={([key]) => key}
						empty={
							<Box>
								No role mappings. Identities get the authenticated or
								unauthenticated role.
							</Box>
						}
						columnDefinitions={[
							{ id: "key", header: "Provider key", cell: ([key]) => key },
							{ id: "type", header: "Type", cell: ([, m]) => m.Type },
							{
								id: "resolution",
								header: "Ambiguous role resolution",
								cell: ([, m]) => m.AmbiguousRoleResolution,
							},
							{
								id: "rules",
								header: "Rules",
								cell: ([, m]) => (
									<div style={{ whiteSpace: "pre-wrap" }}>
										{m.RulesConfiguration?.Rules.map(ruleText).join("\n") ||
											"-"}
									</div>
								),
							},
						]}
					/>
				</SpaceBetween>
			</Container>
			{editing && (
				<RolesDialog
					scope={scope}
					pool={pool}
					roles={roles}
					onClose={() => setEditing(false)}
					onSaved={() => {
						setEditing(false);
						onChanged();
					}}
				/>
			)}
		</>
	);
}
