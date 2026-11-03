import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	Container,
	CopyToClipboard,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { type IdentityScope, identityCall } from "#/lib/cognitoidentity";
import type {
	Credentials,
	IdentityPool,
	PoolRoles,
} from "#/lib/cognitoidentityHelpers";
import { errorText } from "#/lib/consoleActions";
import { unixTime } from "#/lib/format";
import { Copy, Field } from "./Field";

type Issued = { identityId: string; credentials: Credentials };

function Secret({
	label,
	value,
	reveal,
}: {
	label: string;
	value: string;
	reveal: boolean;
}) {
	return (
		<SpaceBetween size="xs">
			<Header
				variant="h3"
				actions={
					<CopyToClipboard
						variant="button"
						textToCopy={value}
						copyButtonText="Copy"
						copySuccessText={`${label} copied`}
						copyErrorText={`${label} failed to copy`}
					/>
				}
			>
				{label}
			</Header>
			<CodeBlock text={reveal ? value : "••••••••••••••••"} />
		</SpaceBetween>
	);
}

// TestSection acts as a client that has not signed in: GetId, then
// GetCredentialsForIdentity, which returns the unauthenticated role's credentials.
export function TestSection({
	scope,
	pool,
	roles,
	enabled,
	onIssued,
}: {
	scope: IdentityScope;
	pool: IdentityPool;
	roles: PoolRoles;
	enabled: boolean;
	onIssued: () => void;
}) {
	const [issued, setIssued] = useState<Issued>();
	const [reveal, setReveal] = useState(false);
	const [error, setError] = useState("");
	const [busy, setBusy] = useState(false);
	const blocked = !pool.AllowUnauthenticatedIdentities
		? "This pool does not allow unauthenticated identities. Edit the pool to allow them."
		: !roles.Roles?.unauthenticated
			? "This pool has no unauthenticated role. Choose Edit roles and set one whose trust policy allows cognito-identity.amazonaws.com."
			: "";
	const run = async () => {
		setBusy(true);
		setError("");
		setIssued(undefined);
		setReveal(false);
		try {
			const id = await identityCall<{ IdentityId: string }>(scope, "GetId", {
				IdentityPoolId: pool.IdentityPoolId,
			});
			const out = await identityCall<{
				IdentityId: string;
				Credentials: Credentials;
			}>(scope, "GetCredentialsForIdentity", { IdentityId: id.IdentityId });
			setIssued({ identityId: out.IdentityId, credentials: out.Credentials });
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
			onIssued();
		}
	};
	const c = issued?.credentials;
	return (
		<Container
			header={
				<Header
					variant="h2"
					description="Gets an identity (GetId) and exchanges it for temporary credentials (GetCredentialsForIdentity), as a client that has not signed in would. Every run adds an identity to this pool."
					actions={
						<Button
							variant="primary"
							loading={busy}
							disabled={!enabled || !!blocked}
							onClick={run}
						>
							Get identity and credentials
						</Button>
					}
				>
					Test unauthenticated credentials
				</Header>
			}
		>
			<SpaceBetween size="m">
				{blocked && <Alert type="warning">{blocked}</Alert>}
				{!!error && (
					<Alert type="error" header="Could not issue credentials">
						{error}
					</Alert>
				)}
				{!issued && !error && !blocked && (
					<Box color="text-body-secondary">
						Choose Get identity and credentials to try this pool.
					</Box>
				)}
				{issued && c && (
					<SpaceBetween size="m">
						<Alert type="info">
							These credentials are kept only until you leave this page.
						</Alert>
						<ColumnLayout columns={3} variant="text-grid">
							<Field label="Identity ID">
								<Copy text={issued.identityId} label="Identity ID" />
							</Field>
							<Field label="Access key ID">
								{c.AccessKeyId ? (
									<Copy text={c.AccessKeyId} label="Access key ID" />
								) : (
									"-"
								)}
							</Field>
							<Field label="Expires">{unixTime(c.Expiration)}</Field>
						</ColumnLayout>
						<Button onClick={() => setReveal(!reveal)}>
							{reveal ? "Hide secrets" : "Reveal secrets"}
						</Button>
						<Secret
							label="Secret access key"
							value={c.SecretKey ?? ""}
							reveal={reveal}
						/>
						<Secret
							label="Session token"
							value={c.SessionToken ?? ""}
							reveal={reveal}
						/>
					</SpaceBetween>
				)}
			</SpaceBetween>
		</Container>
	);
}
