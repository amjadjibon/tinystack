import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	Container,
	Header,
	Link,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ConsoleTags } from "#/components/ConsoleTags";
import { cognitoHref } from "#/lib/cognito";
import { type IdentityScope, identityCall } from "#/lib/cognitoidentity";
import {
	type IdentityPool,
	type PoolRoles,
	poolArn,
	providerEntries,
	userPoolRef,
} from "#/lib/cognitoidentityHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { Copy, Field } from "./Field";
import { IdentitiesSection } from "./IdentitiesSection";
import { DeletePoolDialog, PoolDialog } from "./PoolDialog";
import { RolesSection } from "./RolesSection";
import { TestSection } from "./TestSection";

export function PoolDetails({
	scope,
	pool: id,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: IdentityScope;
	pool: string;
	enabled: boolean;
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const follow = useFollow();
	const [dialog, setDialog] = useState<"" | "edit" | "delete">("");
	// Changing the revision reloads the identities, which the test adds to.
	const [revision, setRevision] = useState(0);
	const poll = usePoll(async () => {
		if (!enabled) return undefined;
		const [pool, roles] = await Promise.all([
			identityCall<IdentityPool>(scope, "DescribeIdentityPool", {
				IdentityPoolId: id,
			}),
			identityCall<PoolRoles>(scope, "GetIdentityPoolRoles", {
				IdentityPoolId: id,
			}),
		]);
		return { pool, roles };
	});
	const pool = poll.data?.pool;
	const roles = poll.data?.roles;
	const arn = poolArn(scope.account, scope.region, id);
	const changed = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!enabled && (
				<Alert type="info">
					Connect a console session to load identity pool details.
				</Alert>
			)}
			<Header
				variant="h1"
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button onClick={poll.reload}>Refresh</Button>
						<Button
							disabled={!enabled || !pool}
							onClick={() => setDialog("edit")}
						>
							Edit
						</Button>
						<Button
							disabled={!enabled || !pool}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Button>
					</SpaceBetween>
				}
			>
				{pool?.IdentityPoolName ?? id}
			</Header>
			{pool && roles && (
				<SpaceBetween size="l">
					<Container header={<Header variant="h2">Configuration</Header>}>
						<ColumnLayout columns={3} variant="text-grid">
							<Field label="Name">{pool.IdentityPoolName}</Field>
							<Field label="Identity pool ID">
								<Copy text={pool.IdentityPoolId} label="Identity pool ID" />
							</Field>
							<Field label="ARN">
								<Copy text={arn} label="ARN" />
							</Field>
							<Field label="Unauthenticated identities">
								{pool.AllowUnauthenticatedIdentities
									? "Allowed"
									: "Not allowed"}
							</Field>
							<Field label="Classic authentication flow">
								{pool.AllowClassicFlow ? "Allowed" : "Not allowed"}
							</Field>
							<Field label="Developer provider">
								{pool.DeveloperProviderName || "-"}
							</Field>
						</ColumnLayout>
					</Container>
					<Table
						variant="container"
						header={<Header variant="h2">Authentication providers</Header>}
						items={providerEntries(pool)}
						trackBy={(e) => `${e.kind}/${e.name}/${e.detail}`}
						empty={
							<Box>
								No authentication providers. Choose Edit to add a Cognito user
								pool, OpenID Connect or developer provider.
							</Box>
						}
						columnDefinitions={[
							{ id: "kind", header: "Type", cell: (e) => e.kind },
							{
								id: "provider",
								header: "Provider",
								cell: (e) => {
									const ref =
										e.kind === "Cognito user pool"
											? userPoolRef(e.name)
											: undefined;
									return ref ? (
										<Link
											href={cognitoHref({
												account: scope.account,
												region: ref.region,
												id: ref.id,
											})}
											onFollow={follow}
										>
											{e.name}
										</Link>
									) : (
										e.name
									);
								},
							},
							{ id: "detail", header: "Details", cell: (e) => e.detail },
						]}
					/>
					<RolesSection
						scope={scope}
						pool={pool}
						roles={roles}
						enabled={enabled}
						onChanged={changed}
					/>
					<IdentitiesSection
						key={revision}
						scope={scope}
						pool={id}
						enabled={enabled}
						onChanged={onChanged}
					/>
					<TestSection
						scope={scope}
						pool={pool}
						roles={roles}
						enabled={enabled}
						onIssued={() => {
							setRevision((r) => r + 1);
							onChanged();
						}}
					/>
					<ConsoleTags
						tags={pool.IdentityPoolTags ?? {}}
						enabled={enabled}
						onChanged={changed}
						plan={(next, removed) => [
							...(Object.keys(next).length
								? [
										{
											label: "TagResource",
											run: () =>
												identityCall(scope, "TagResource", {
													ResourceArn: arn,
													Tags: next,
												}),
										},
									]
								: []),
							...(removed.length
								? [
										{
											label: "UntagResource",
											run: () =>
												identityCall(scope, "UntagResource", {
													ResourceArn: arn,
													TagKeys: removed,
												}),
										},
									]
								: []),
						]}
					/>
				</SpaceBetween>
			)}
			{dialog === "edit" && pool && (
				<PoolDialog
					scope={scope}
					pool={pool}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						changed();
					}}
				/>
			)}
			{dialog === "delete" && pool && (
				<DeletePoolDialog
					scope={scope}
					pool={pool}
					onClose={() => setDialog("")}
					onDeleted={() => {
						setDialog("");
						onDeleted();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
