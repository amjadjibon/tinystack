import { Alert, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { LoadBalancers } from "#/components/elbv2/LoadBalancers";
import { TargetGroups } from "#/components/elbv2/TargetGroups";
import {
	type ELBv2Scope,
	elbv2Href,
	listSecurityGroups,
	listSubnets,
	listTargetGroups,
	listVpcs,
} from "#/lib/elbv2";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/elbv2")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: ELBv2Page,
});

function ELBv2Page() {
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.elbv2 &&
		!!capabilities.data?.pages.ec2 &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = elbv2Href(scope);
	return (
		<ConsoleLayout
			navigationHeader={{ text: "Elastic Load Balancing", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Load balancers", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[{ text: "Elastic Load Balancing", href: home }]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.elbv2 && (
						<Alert type="warning">
							Elastic Load Balancing is not enabled on this server.
						</Alert>
					)}
					{capabilities.data?.pages.elbv2 && !capabilities.data.pages.ec2 && (
						<Alert type="warning">
							EC2 is not enabled, so subnets, security groups and VPCs cannot be
							listed.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<Workspace
						key={`${account}/${region}/${enabled}`}
						scope={scope}
						enabled={enabled}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}

function Workspace({
	scope,
	enabled,
	onChanged,
}: {
	scope: ELBv2Scope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [tab, setTab] = useState("balancers");
	const groups = usePoll(
		() => (enabled ? listTargetGroups(scope) : Promise.resolve([])),
		0,
	);
	const subnets = usePoll(
		() => (enabled ? listSubnets(scope) : Promise.resolve([])),
		0,
	);
	const securityGroups = usePoll(
		() => (enabled ? listSecurityGroups(scope) : Promise.resolve([])),
		0,
	);
	const vpcs = usePoll(
		() => (enabled ? listVpcs(scope) : Promise.resolve([])),
		0,
	);
	const refresh = () => {
		groups.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{[subnets.error, securityGroups.error, vpcs.error]
				.filter((e) => e)
				.map((e) => (
					<Alert key={e} type="error">
						{e}
					</Alert>
				))}
			<Tabs
				activeTabId={tab}
				onChange={({ detail }) => setTab(detail.activeTabId)}
				tabs={[
					{
						id: "balancers",
						label: "Load balancers",
						content: (
							<LoadBalancers
								scope={scope}
								enabled={enabled}
								subnets={subnets.data ?? []}
								securityGroups={securityGroups.data ?? []}
								groups={groups.data ?? []}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "groups",
						label: "Target groups",
						content: (
							<TargetGroups
								scope={scope}
								enabled={enabled}
								vpcs={vpcs.data ?? []}
								groups={groups.data ?? []}
								loading={groups.loading}
								error={groups.error}
								onChanged={refresh}
							/>
						),
					},
				]}
			/>
		</SpaceBetween>
	);
}
