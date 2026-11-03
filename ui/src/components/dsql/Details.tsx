import {
	Alert,
	Box,
	CopyToClipboard,
	Header,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { CodeBlock } from "#/components/CodeBlock";
import { api } from "#/lib/api";
import {
	type Cluster,
	caCommand,
	type DSQLScope,
	needsPeer,
	peerArns,
	proxyPort,
	psqlCommand,
	statusType,
	tokenCommand,
} from "#/lib/dsqlHelpers";
import { unixTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";

const copy = (value: string) => (
	<CopyToClipboard
		variant="inline"
		textToCopy={value}
		copySuccessText="Copied"
		copyErrorText="Copy failed"
	/>
);

export function ClusterStatus({ status }: { status?: string }) {
	return (
		<StatusIndicator type={statusType(status)}>{status ?? "-"}</StatusIndicator>
	);
}

// Snippet shows one command people copy into a terminal.
function Snippet({ title, text }: { title: string; text: string }) {
	return (
		<SpaceBetween size="xs">
			<Header
				variant="h3"
				actions={
					<CopyToClipboard
						variant="icon"
						textToCopy={text}
						copyButtonAriaLabel={`Copy: ${title}`}
						copySuccessText="Copied"
						copyErrorText="Copy failed"
					/>
				}
			>
				{title}
			</Header>
			<CodeBlock text={text} />
		</SpaceBetween>
	);
}

export function Details({
	scope,
	cluster: c,
	docker,
}: {
	scope: DSQLScope;
	cluster: Cluster;
	docker: boolean;
}) {
	const active = c.Status === "ACTIVE";
	// The proxy's loopback port is reported in the inventory, once the cluster is
	// ACTIVE. The page remounts this view when the status changes, which loads it.
	const proxy = usePoll(
		async () =>
			docker && active ? proxyPort(await api.resources(), c.Arn) : undefined,
		0,
	);
	const endpoint = c.Endpoint ?? "";
	const token = tokenCommand(endpoint, scope.region);
	const command = proxy.data ? psqlCommand(endpoint, proxy.data) : "";
	return (
		<SpaceBetween size="m">
			<div>ARN: {copy(c.Arn)}</div>
			<div>Identifier: {copy(c.Identifier)}</div>
			<div>
				Status: <ClusterStatus status={c.Status} /> · Created:{" "}
				{unixTime(c.CreationTime)}
			</div>
			<div>
				Deletion protection: {c.DeletionProtectionEnabled ? "On" : "Off"}
			</div>
			<div>
				Encryption: {c.EncryptionDetails?.EncryptionType ?? "-"} ·{" "}
				{c.EncryptionDetails?.EncryptionStatus ?? "-"}
			</div>
			{!!c.MultiRegionProperties && (
				<SpaceBetween size="xxs">
					<div>
						Witness Region: {c.MultiRegionProperties.WitnessRegion ?? "-"}
					</div>
					{peerArns(c).map((arn) => (
						<div key={arn}>Peer cluster: {copy(arn)}</div>
					))}
				</SpaceBetween>
			)}
			{!!c.MultiRegionProperties && c.Status === "PENDING_SETUP" && (
				<Alert header="Waiting for the peer cluster">
					{needsPeer(c)
						? "Create the peer cluster in another Region with this cluster's ARN, then choose Edit settings and enter the peer's ARN."
						: "The cluster is ACTIVE once the peer also lists this cluster as its peer. Edit the peer and enter this cluster's ARN."}
				</Alert>
			)}
			<SpaceBetween size="xxs">
				<div>Endpoint: {endpoint ? copy(endpoint) : "-"}</div>
				{!!proxy.data && (
					<div>Local proxy address: {copy(`127.0.0.1:${proxy.data}`)}</div>
				)}
			</SpaceBetween>
			{docker && !active && (
				<Alert>
					Connection details are available once the cluster is ACTIVE.
				</Alert>
			)}
			{!!proxy.error && <Alert type="error">{proxy.error}</Alert>}
			<SpaceBetween size="s">
				<Header variant="h3">Authentication</Header>
				<Box>
					Aurora DSQL has no database passwords. The password is a short-lived
					IAM auth token that the AWS CLI or an SDK creates on your machine from
					your credentials; this console never creates one. Create it for the
					cluster endpoint above, never the loopback address, and pass it as
					PGPASSWORD. Database roles other than admin use the non-admin token,
					for example aws dsql generate-db-connect-auth-token.
				</Box>
				{!!token && <Snippet title="Create an admin auth token" text={token} />}
				<Box>
					Every AWS SDK has the same helper, such as
					feature/dsql/auth.GenerateDBConnectAdminAuthToken in the Go SDK or
					generate_db_connect_admin_auth_token in boto3.
				</Box>
			</SpaceBetween>
			{!!command && (
				<SpaceBetween size="s">
					<Snippet title="Export the local CA, once" text={caCommand} />
					<Snippet title="Connect with psql" text={command} />
				</SpaceBetween>
			)}
		</SpaceBetween>
	);
}
