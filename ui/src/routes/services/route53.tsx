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
import { HealthChecks } from "#/components/route53/HealthChecks";
import { ZoneDetails } from "#/components/route53/ZoneDetails";
import { CreateZoneDialog } from "#/components/route53/ZoneForms";
import {
	type HostedZone,
	type Route53Scope,
	route53Href,
	route53Pages,
} from "#/lib/route53";
import { bareZoneId } from "#/lib/route53Helpers";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/route53")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		zone: String(search.zone ?? ""),
		section: search.section === "healthchecks" ? "healthchecks" : "zones",
	}),
	component: Route53Page,
});

function Route53Page() {
	const search = Route.useSearch(),
		{ canWrite, account, inventory, capabilities } = useScope();
	const enabled =
		!!capabilities.data?.route53 && canWrite && /^\d{12}$/.test(account);
	const scope: Route53Scope = { account, zone: search.zone || undefined };
	const list = route53Href({ account });
	const checks = `${list}&section=healthchecks`;
	const vpcs = (inventory.data ?? []).filter(
		(r) => r.service === "ec2" && r.type === "vpc" && r.account === account,
	);
	return (
		<ConsoleLayout
			contentType={search.zone ? "default" : "table"}
			navigationHeader={{ text: "Route 53", href: list }}
			activeHref={search.section === "healthchecks" ? checks : list}
			navigationItems={[
				{ type: "link", text: "Hosted zones", href: list },
				{ type: "link", text: "Health checks", href: checks },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
				{ type: "link", text: "Delivery log", href: "/deliveries" },
			]}
			breadcrumbs={[
				{ text: "Route 53", href: list },
				search.section === "healthchecks"
					? { text: "Health checks", href: checks }
					: { text: "Hosted zones", href: list },
				...(search.zone
					? [{ text: search.zone, href: route53Href(scope) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info" header="Route 53 management is read-only">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.route53 && (
						<Alert type="warning">
							Route 53 is not enabled on this server.
						</Alert>
					)}
					{search.section === "healthchecks" ? (
						<HealthChecks
							key={`${account}/${enabled}`}
							scope={scope}
							enabled={enabled}
							onChanged={inventory.reload}
						/>
					) : (
						<ZoneWorkspace
							key={`${account}/${search.zone}/${enabled}`}
							scope={scope}
							enabled={enabled}
							vpcs={vpcs}
							onChanged={inventory.reload}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}

function ZoneWorkspace({
	scope,
	enabled,
	vpcs,
	onChanged,
}: {
	scope: Route53Scope;
	enabled: boolean;
	vpcs: Parameters<typeof CreateZoneDialog>[0]["vpcs"];
	onChanged: () => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow(),
		[create, setCreate] = useState(false);
	const zones = usePoll(
		() =>
			enabled
				? route53Pages<HostedZone>(scope, "ListHostedZones", "HostedZones")
				: Promise.resolve([] as HostedZone[]),
		5000,
	);
	const all = zones.data ?? [];
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(all, {
			filtering: {
				empty: <Box>No hosted zones in this account.</Box>,
				noMatch: <Box>No matching hosted zones.</Box>,
				filteringFunction: (z, text) =>
					`${z.Name} ${z.Id} ${z.Config?.Comment ?? ""}`
						.toLowerCase()
						.includes(text.toLowerCase()),
			},
			sorting: { defaultState: { sortingColumn: { sortingField: "Name" } } },
			pagination: { pageSize: 20 },
		});
	const zone = scope.zone
		? all.find((z) => bareZoneId(z.Id) === scope.zone)
		: undefined;
	const account = scope.account;
	const back = () => navigate({ href: route53Href({ account }) });
	if (scope.zone) {
		if (zones.loading) return <Box>Loading hosted zone</Box>;
		if (!zone)
			return (
				<Alert
					type="warning"
					header="Hosted zone not found"
					action={<Button onClick={back}>Back to hosted zones</Button>}
				>
					No hosted zone {scope.zone} exists in this account.
				</Alert>
			);
		return (
			<ZoneDetails
				scope={scope}
				zone={zone}
				enabled={enabled}
				onChanged={() => {
					zones.reload();
					onChanged();
				}}
				onDeleted={() => {
					zones.reload();
					onChanged();
					back();
				}}
			/>
		);
	}
	return (
		<SpaceBetween size="m">
			{zones.error && (
				<Alert type="error" header="Cannot load hosted zones">
					{zones.error}
				</Alert>
			)}
			<Table
				{...collectionProps}
				items={items}
				trackBy="Id"
				loading={zones.loading}
				loadingText="Loading hosted zones"
				variant="full-page"
				header={
					<Header
						variant="awsui-h1-sticky"
						counter={`(${all.length})`}
						description="Manage DNS hosted zones and their record sets."
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh hosted zones"
									onClick={zones.reload}
								/>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => setCreate(true)}
								>
									Create hosted zone
								</Button>
							</SpaceBetween>
						}
					>
						Hosted zones
					</Header>
				}
				filter={
					<TextFilter
						{...filterProps}
						filteringPlaceholder="Find hosted zones"
					/>
				}
				pagination={<Pagination {...paginationProps} />}
				columnDefinitions={[
					{
						id: "name",
						header: "Domain name",
						cell: (z) => (
							<Link
								href={route53Href({ account, zone: bareZoneId(z.Id) })}
								onFollow={follow}
							>
								{z.Name}
							</Link>
						),
						sortingField: "Name",
						isRowHeader: true,
					},
					{
						id: "type",
						header: "Type",
						cell: (z) => (z.Config?.PrivateZone ? "Private" : "Public"),
					},
					{
						id: "records",
						header: "Record sets",
						cell: (z) => z.ResourceRecordSetCount ?? 0,
					},
					{
						id: "comment",
						header: "Comment",
						cell: (z) => z.Config?.Comment || "—",
					},
					{ id: "id", header: "Hosted zone ID", cell: (z) => bareZoneId(z.Id) },
				]}
			/>
			{create && (
				<CreateZoneDialog
					scope={scope}
					vpcs={vpcs}
					onClose={() => setCreate(false)}
					onSaved={(id) => {
						setCreate(false);
						zones.reload();
						onChanged();
						if (id) navigate({ href: route53Href({ account, zone: id }) });
					}}
				/>
			)}
		</SpaceBetween>
	);
}
