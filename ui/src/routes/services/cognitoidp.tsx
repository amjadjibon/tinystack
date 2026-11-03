import {
	Alert,
	Button,
	Header,
	Link,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import {
	CognitoClients,
	CognitoDomain,
} from "#/components/cognito/CognitoApplications";
import { CognitoUsers } from "#/components/cognito/CognitoDetails";
import { CognitoGroups } from "#/components/cognito/CognitoGroups";
import {
	PoolActions,
	PoolSettings,
} from "#/components/cognito/CognitoManagement";
import { ResourceTable } from "#/components/ResourceTable";
import {
	type CognitoScope,
	cognitoCall,
	cognitoHref,
	cognitoPages,
	type Pool,
} from "#/lib/cognito";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/cognitoidp")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		id: String(s.id ?? ""),
		username: String(s.username ?? ""),
	}),
	component: CognitoPage,
});
function CognitoPage() {
	const search = Route.useSearch(),
		{ account, region, canWrite } = useScope();
	const scope = { account, region, id: search.id };
	return (
		<ConsoleLayout
			contentType="table"
			navigationHeader={{
				text: "Cognito",
				href: cognitoHref({ account, region }),
			}}
			navigationItems={[
				{
					type: "link",
					text: "User pools",
					href: cognitoHref({ account, region }),
				},
				{ type: "link", text: "Mailbox", href: "/mailbox" },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "Cognito", href: cognitoHref({ account, region }) },
			]}
			content={
				<Workspace
					key={`${account}/${region}/${search.id}/${canWrite}`}
					scope={scope}
					enabled={canWrite}
					username={search.username}
				/>
			}
		/>
	);
}
function Workspace({
	scope,
	enabled,
	username,
}: {
	scope: CognitoScope;
	enabled: boolean;
	username: string;
}) {
	const [tab, setTab] = useState("users");
	const pools = usePoll(async () => {
		if (!enabled) return [];
		const list = await cognitoPages<Pool>(scope, "ListUserPools", "UserPools");
		return Promise.all(
			list.map(
				async (p) =>
					(await cognitoCall({ ...scope, id: p.Id }, "DescribeUserPool", {}))
						.UserPool,
			),
		);
	});
	const pool = pools.data?.find((p) => p.Id === scope.id);
	return (
		<SpaceBetween size="l">
			{!enabled && (
				<Alert type="info">
					Connect a console session for this account to manage user pools.
				</Alert>
			)}
			{!scope.id && (
				<PoolActions
					scope={scope}
					pool={pool}
					enabled={enabled}
					onChanged={pools.reload}
				/>
			)}
			{pools.error && <Alert type="error">{pools.error}</Alert>}
			{scope.id ? (
				<SpaceBetween size="l">
					<Header
						actions={
							<SpaceBetween direction="horizontal" size="s">
								<PoolActions
									scope={scope}
									pool={pool}
									enabled={enabled}
									onChanged={pools.reload}
								/>
								<Button href={cognitoHref({ ...scope, id: undefined })}>
									User pools
								</Button>
							</SpaceBetween>
						}
					>
						{pool?.Name ?? scope.id}
					</Header>
					<Tabs
						activeTabId={tab}
						onChange={({ detail }) => setTab(detail.activeTabId)}
						tabs={[
							{
								id: "settings",
								label: "Settings and tags",
								content: <PoolSettings scope={scope} enabled={enabled} />,
							},
							{
								id: "users",
								label: "Users",
								content:
									tab === "users" ? (
										<CognitoUsers
											scope={scope}
											enabled={enabled}
											initialUser={username}
										/>
									) : null,
							},
							{
								id: "groups",
								label: "Groups",
								content: <CognitoGroups scope={scope} enabled={enabled} />,
							},
							{
								id: "clients",
								label: "App clients",
								content:
									tab === "clients" ? (
										<CognitoClients scope={scope} enabled={enabled} />
									) : null,
							},
							{
								id: "domain",
								label: "Domain",
								content: pool ? (
									<CognitoDomain
										key={pool.CustomDomain || pool.Domain}
										scope={scope}
										pool={pool}
										enabled={enabled}
										onChanged={pools.reload}
									/>
								) : (
									<Alert>Loading pool details.</Alert>
								),
							},
						]}
					/>
				</SpaceBetween>
			) : (
				<ResourceTable
					title="Cognito user pools"
					emptyText="Choose Create user pool, or use the AWS CLI or an SDK."
					loading={pools.loading}
					onRefresh={pools.reload}
					resources={(pools.data ?? []).map((p) => ({
						service: "cognitoidp",
						type: "user-pool",
						name: p.Name,
						arn: `arn:aws:cognito-idp:${scope.region}:${scope.account}:userpool/${p.Id}`,
						account: scope.account,
						region: scope.region,
						details: p,
					}))}
					extraColumns={[
						{
							id: "id",
							header: "Pool ID",
							cell: (r) => (
								<Link
									href={cognitoHref({ ...scope, id: String(r.details?.Id) })}
								>
									{String(r.details?.Id)}
								</Link>
							),
						},
						{
							id: "created",
							header: "Created",
							cell: (r) =>
								r.details?.CreationDate
									? new Date(
											Number(r.details.CreationDate) * 1000,
										).toLocaleString()
									: "—",
						},
						{
							id: "users",
							header: "Estimated users",
							cell: (r) => String(r.details?.EstimatedNumberOfUsers ?? "—"),
						},
						{
							id: "mfa",
							header: "MFA",
							cell: (r) => String(r.details?.MfaConfiguration ?? "—"),
						},
					]}
				/>
			)}
		</SpaceBetween>
	);
}
