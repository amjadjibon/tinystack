import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Header,
	Link,
	Pagination,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { DatabaseActions } from "#/components/rds/DatabaseActions";
import {
	DatabaseDetails,
	DatabaseStatus,
} from "#/components/rds/DatabaseDetails";
import { DatabaseForm } from "#/components/rds/DatabaseForm";
import { DatabaseGroups } from "#/components/rds/DatabaseGroups";
import {
	type DBInstance,
	type RDSScope,
	rdsDate,
	rdsHref,
	rdsPages,
} from "#/lib/rds";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/rds")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		database: String(s.database ?? ""),
		section:
			s.section === "subnets" || s.section === "parameters"
				? s.section
				: "databases",
	}),
	component: RDSPage,
});
function RDSPage() {
	const search = Route.useSearch(),
		{ account, region, capabilities, inventory } = useScope();
	const scope = {
		account,
		region,
		database: search.database,
		section: search.section,
	};
	const enabled =
		!!capabilities.data?.rds &&
		!capabilities.data.requires_signature &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const list = rdsHref({ account, region });
	const sections = [
		{ id: "databases", text: "Databases" },
		{ id: "subnets", text: "Subnet groups" },
		{ id: "parameters", text: "Parameter groups" },
	];
	return (
		<ConsoleLayout
			contentType={search.database ? "default" : "table"}
			navigationHeader={{ text: "RDS", href: list }}
			activeHref={rdsHref({ account, region, section: search.section })}
			navigationItems={[
				...sections.map((s) => ({
					type: "link" as const,
					text: s.text,
					href: rdsHref({ account, region, section: s.id }),
				})),
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "RDS", href: list },
				{
					text:
						sections.find((s) => s.id === search.section)?.text ?? "Databases",
					href: rdsHref({ account, region, section: search.section }),
				},
				...(search.database
					? [{ text: search.database, href: rdsHref(scope) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{capabilities.error && (
						<Alert type="error">{capabilities.error}</Alert>
					)}
					{capabilities.data?.requires_signature && (
						<Alert type="info">
							RDS management requires signed AWS CLI or SDK requests in verify
							mode. Database metadata inventory remains available.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.rds && (
						<Alert type="warning">RDS is not enabled on this server.</Alert>
					)}
					<RDSWorkspace
						key={`${account}/${region}/${search.section}/${search.database}/${enabled}`}
						scope={scope}
						enabled={enabled}
						fallback={(inventory.data ?? [])
							.filter(
								(r) =>
									r.service === "rds" &&
									r.type === "db-instance" &&
									r.account === account &&
									r.region === region,
							)
							.map((r) => ({
								DBInstanceIdentifier: r.name,
								DBInstanceArn: r.arn,
								Engine: String(r.details?.Engine ?? ""),
								DBInstanceStatus: String(r.details?.Status ?? ""),
								EngineVersion: "",
								DBInstanceClass: "",
								MasterUsername: "",
								TagList: Object.entries(r.tags ?? {}).map(([Key, Value]) => ({
									Key,
									Value,
								})),
							}))}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}
function RDSWorkspace({
	scope,
	enabled,
	fallback,
	onChanged,
}: {
	scope: RDSScope;
	enabled: boolean;
	fallback: DBInstance[];
	onChanged: () => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow(),
		[create, setCreate] = useState(false);
	const poll = usePoll(() =>
		enabled && scope.section === "databases"
			? rdsPages<DBInstance>(
					scope,
					"DescribeDBInstances",
					"DBInstances",
					scope.database ? { DBInstanceIdentifier: scope.database } : {},
				)
			: Promise.resolve(undefined),
	);
	const databases = poll.data ?? fallback;
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(databases, {
			filtering: {
				empty: <Box>No databases in this account and Region.</Box>,
				noMatch: <Box>No matching databases.</Box>,
			},
			sorting: {
				defaultState: {
					sortingColumn: { sortingField: "DBInstanceIdentifier" },
				},
			},
			pagination: { pageSize: 20 },
			selection: { trackBy: "DBInstanceIdentifier" },
		});
	const selectedID = collectionProps.selectedItems?.[0]?.DBInstanceIdentifier;
	const selected = databases.find((d) => d.DBInstanceIdentifier === selectedID),
		detail = databases.find((d) => d.DBInstanceIdentifier === scope.database);
	const changed = () => {
		poll.reload();
		onChanged();
	};
	const deleted = () => {
		navigate({
			href: rdsHref({ account: scope.account, region: scope.region }),
		});
	};
	if (scope.section !== "databases")
		return <DatabaseGroups {...{ scope, enabled, onChanged }} />;
	return (
		<SpaceBetween size="l">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			{scope.database ? (
				detail ? (
					enabled ? (
						<DatabaseDetails
							{...{ scope, enabled }}
							instance={detail}
							onChanged={changed}
							onDeleted={deleted}
						/>
					) : (
						<>
							<Header variant="h1">{detail.DBInstanceIdentifier}</Header>
							<DatabaseStatus status={detail.DBInstanceStatus} />
							<Box>Engine: {detail.Engine}</Box>
						</>
					)
				) : (
					!poll.loading &&
					!poll.error && (
						<Alert type="warning">Database not found in this scope.</Alert>
					)
				)
			) : (
				<Table
					{...collectionProps}
					items={items}
					selectionType="single"
					trackBy="DBInstanceIdentifier"
					loading={poll.loading}
					loadingText="Loading databases"
					variant="full-page"
					header={
						<Header
							variant="awsui-h1-sticky"
							counter={`(${databases.length})`}
							actions={
								<SpaceBetween direction="horizontal" size="xs">
									<Button
										iconName="refresh"
										ariaLabel="Refresh databases"
										onClick={poll.reload}
									/>
									<DatabaseActions
										scope={scope}
										instance={selected}
										enabled={enabled}
										onChanged={changed}
										onDeleted={() => {}}
									/>
									<Button
										variant="primary"
										disabled={!enabled}
										onClick={() => setCreate(true)}
									>
										Create database
									</Button>
								</SpaceBetween>
							}
						>
							Databases
						</Header>
					}
					filter={
						<TextFilter
							{...filterProps}
							filteringPlaceholder="Find databases"
						/>
					}
					pagination={<Pagination {...paginationProps} />}
					columnDefinitions={[
						{
							id: "identifier",
							header: "DB identifier",
							sortingField: "DBInstanceIdentifier",
							isRowHeader: true,
							cell: (d) => (
								<Link
									href={rdsHref({ ...scope, database: d.DBInstanceIdentifier })}
									onFollow={follow}
								>
									{d.DBInstanceIdentifier}
								</Link>
							),
						},
						{
							id: "engine",
							header: "Engine and version",
							cell: (d) => `${d.Engine} ${d.EngineVersion}`,
						},
						{
							id: "status",
							header: "Status",
							cell: (d) => <DatabaseStatus status={d.DBInstanceStatus} />,
						},
						{
							id: "class",
							header: "Class",
							cell: (d) => d.DBInstanceClass || "—",
						},
						{
							id: "endpoint",
							header: "Endpoint",
							cell: (d) =>
								d.Endpoint?.Port
									? `${d.Endpoint.Address}:${d.Endpoint.Port}`
									: "—",
						},
						{
							id: "created",
							header: "Created",
							sortingField: "InstanceCreateTime",
							cell: (d) => rdsDate(d.InstanceCreateTime),
						},
					]}
				/>
			)}
			{create && enabled && (
				<DatabaseForm
					scope={scope}
					onClose={() => setCreate(false)}
					onSaved={(id) => {
						setCreate(false);
						changed();
						navigate({ href: rdsHref({ ...scope, database: id }) });
					}}
				/>
			)}
		</SpaceBetween>
	);
}
