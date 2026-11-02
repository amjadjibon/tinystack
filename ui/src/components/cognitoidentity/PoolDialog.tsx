import {
	Alert,
	Autosuggest,
	Button,
	Checkbox,
	ExpandableSection,
	FormField,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Text } from "#/components/memorydb/Fields";
import { cognitoPages } from "#/lib/cognito";
import { type IdentityScope, identityCall } from "#/lib/cognitoidentity";
import {
	emptyPoolForm,
	emptyProviderRow,
	formFromPool,
	type IdentityPool,
	type PoolForm,
	type ProviderRow,
	poolInput,
	socialProviders,
	userPoolProvider,
} from "#/lib/cognitoidentityHelpers";
import { usePoll } from "#/lib/usePoll";

// PoolDialog creates an identity pool, or edits pool, whose settings it replaces.
export function PoolDialog({
	scope,
	pool,
	onClose,
	onSaved,
}: {
	scope: IdentityScope;
	pool?: IdentityPool;
	onClose: () => void;
	onSaved: (id: string) => void;
}) {
	const [form, setForm] = useState(() =>
		pool ? formFromPool(pool) : emptyPoolForm(),
	);
	const saved = useRef(pool?.IdentityPoolId ?? "");
	const operation = pool ? "UpdateIdentityPool" : "CreateIdentityPool";
	// Existing user pools are offered as providers; without the user pools
	// service the rows still accept a typed provider name.
	const userPools = usePoll(async () => {
		const found = await cognitoPages<{ Id: string; Name: string }>(
			scope,
			"ListUserPools",
			"UserPools",
		);
		return found.map((p) => ({
			value: userPoolProvider(scope.region, p.Id),
			label: p.Name,
			description: p.Id,
		}));
	}, 0);
	const set = <K extends keyof PoolForm>(key: K, value: PoolForm[K]) =>
		setForm((f) => ({ ...f, [key]: value }));
	const changeRows = (change: (rows: ProviderRow[]) => ProviderRow[]) =>
		setForm((f) => ({ ...f, userPools: change(f.userPools) }));
	const setRow = (id: string, patch: Partial<ProviderRow>) =>
		changeRows((rows) =>
			rows.map((r) => (r.id === id ? { ...r, ...patch } : r)),
		);
	return (
		<ActionDialog
			title={pool ? `Edit ${pool.IdentityPoolName}` : "Create identity pool"}
			submit={pool ? "Save identity pool" : "Create identity pool"}
			onClose={onClose}
			onSaved={() => onSaved(saved.current)}
			plan={async () => {
				const input = poolInput(form, pool?.IdentityPoolId);
				return [
					{
						label: operation,
						run: async () => {
							const out = await identityCall<IdentityPool>(
								scope,
								operation,
								input,
							);
							saved.current = out.IdentityPoolId;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{pool && (
					<Alert type="info">
						Saving replaces every setting below. Providers you remove are
						cleared. Tags and roles are edited on the pool's page.
					</Alert>
				)}
				<Text
					label="Name"
					value={form.name}
					onChange={(v) => set("name", v)}
					constraint="1 to 128 letters, digits, spaces and + = , . @ - _ characters."
				/>
				<Checkbox
					checked={form.allowUnauthenticated}
					onChange={({ detail }) => set("allowUnauthenticated", detail.checked)}
					description="Clients can get an identity and credentials without signing in. They receive the pool's unauthenticated role."
				>
					Allow unauthenticated identities
				</Checkbox>
				<Checkbox
					checked={form.allowClassic}
					onChange={({ detail }) => set("allowClassic", detail.checked)}
					description="Also allow GetOpenIdToken followed by AssumeRoleWithWebIdentity."
				>
					Allow the classic (basic) authentication flow
				</Checkbox>
				<Text
					label="Developer provider name (optional)"
					value={form.developerProvider}
					disabled={!!pool?.DeveloperProviderName}
					onChange={(v) => set("developerProvider", v)}
					description="Names the login your backend uses with GetOpenIdTokenForDeveloperIdentity."
					constraint={
						pool?.DeveloperProviderName
							? "A developer provider name cannot be changed once it is set."
							: "1 to 128 letters, digits, periods, hyphens and underscores."
					}
				/>
				<SpaceBetween size="s">
					{form.userPools.map((row, i) => (
						<SpaceBetween key={row.id} size="xs">
							<FormField
								label={`Cognito user pool provider ${i + 1}`}
								description="Pick a user pool, or type cognito-idp.<region>.amazonaws.com/<user pool ID>."
							>
								<Autosuggest
									ariaLabel={`Cognito user pool provider ${i + 1}`}
									value={row.name}
									placeholder="cognito-idp.us-east-1.amazonaws.com/us-east-1_AbCdEfGhI"
									options={userPools.data ?? []}
									enteredTextLabel={(text) => `Use provider: ${text}`}
									empty="No matching user pools. Type a provider name instead."
									onChange={({ detail }) =>
										setRow(row.id, { name: detail.value })
									}
								/>
							</FormField>
							<Text
								label={`App client ID ${i + 1} (optional)`}
								value={row.clientId}
								onChange={(v) => setRow(row.id, { clientId: v })}
								description="Leave empty to accept tokens from any app client of the user pool."
							/>
							<Checkbox
								checked={row.serverSideCheck}
								onChange={({ detail }) =>
									setRow(row.id, { serverSideCheck: detail.checked })
								}
								description="Stored with the pool. TinyStack verifies user pool tokens either way."
							>
								Server-side token check
							</Checkbox>
							<Button
								variant="inline-link"
								ariaLabel={`Remove user pool provider ${i + 1}`}
								onClick={() =>
									changeRows((rows) => rows.filter((r) => r.id !== row.id))
								}
							>
								Remove provider
							</Button>
						</SpaceBetween>
					))}
					<Button
						onClick={() => changeRows((rows) => [...rows, emptyProviderRow()])}
					>
						Add Cognito user pool provider
					</Button>
				</SpaceBetween>
				<FormField
					label="OpenID Connect provider ARNs (optional)"
					description="One IAM OpenID Connect provider ARN per line, such as arn:aws:iam::000000000000:oidc-provider/example.com."
				>
					<Textarea
						ariaLabel="OpenID Connect provider ARNs"
						rows={3}
						value={form.oidc}
						onChange={({ detail }) => set("oidc", detail.value)}
					/>
				</FormField>
				<ExpandableSection
					headerText="Social login providers (optional)"
					defaultExpanded={Object.values(form.social).some(Boolean)}
				>
					<SpaceBetween size="s">
						<Alert type="info">
							TinyStack keeps these app IDs but does not verify these providers'
							tokens, so a client that signs in with one gets an error. Use a
							user pool, an OpenID Connect provider or developer authentication
							to authenticate.
						</Alert>
						{socialProviders.map((provider) => (
							<Text
								key={provider}
								label={`${provider} app ID`}
								value={form.social[provider] ?? ""}
								onChange={(v) =>
									setForm((f) => ({
										...f,
										social: { ...f.social, [provider]: v },
									}))
								}
							/>
						))}
					</SpaceBetween>
				</ExpandableSection>
				{!pool && (
					<FormField
						label="Tags (JSON)"
						description='Optional object of string values, such as {"env": "dev"}.'
					>
						<Textarea
							ariaLabel="Identity pool tags"
							rows={3}
							value={form.tags}
							onChange={({ detail }) => set("tags", detail.value)}
						/>
					</FormField>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}

export function DeletePoolDialog({
	scope,
	pool,
	onClose,
	onDeleted,
}: {
	scope: IdentityScope;
	pool: IdentityPool;
	onClose: () => void;
	onDeleted: () => void;
}) {
	return (
		<ActionDialog
			title="Delete identity pool"
			submit="Delete identity pool"
			confirmation={pool.IdentityPoolName}
			onClose={onClose}
			onSaved={onDeleted}
			plan={async () => [
				{
					label: "DeleteIdentityPool",
					run: () =>
						identityCall(scope, "DeleteIdentityPool", {
							IdentityPoolId: pool.IdentityPoolId,
						}),
				},
			]}
		>
			<Alert type="warning" header="Its identities are deleted too">
				Deleting {pool.IdentityPoolName} permanently removes the pool, its roles
				and every identity in it. Clients that use the pool can no longer get
				credentials.
			</Alert>
		</ActionDialog>
	);
}
