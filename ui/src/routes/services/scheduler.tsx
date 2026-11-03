import { Alert, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { SchedulerGroups } from "#/components/scheduler/SchedulerGroups";
import { SchedulerSchedules } from "#/components/scheduler/SchedulerSchedules";
import { validRegion } from "#/lib/s3";
import {
	listGroups,
	listSchedules,
	type SchedulerScope,
	schedulerHref,
} from "#/lib/scheduler";
import { useScope } from "#/lib/scope";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/scheduler")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: SchedulerPage,
});

function SchedulerPage() {
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.pages.scheduler &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = schedulerHref(scope);
	return (
		<ConsoleLayout
			navigationHeader={{ text: "EventBridge Scheduler", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Schedules", href: home },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[{ text: "EventBridge Scheduler", href: home }]}
			content={
				<SpaceBetween size="l">
					{(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.scheduler && (
						<Alert type="warning">
							EventBridge Scheduler is not enabled on this server.
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
						resources={inventory.data ?? []}
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
	resources,
	onChanged,
}: {
	scope: SchedulerScope;
	enabled: boolean;
	resources: Parameters<typeof SchedulerSchedules>[0]["resources"];
	onChanged: () => void;
}) {
	const [tab, setTab] = useState("schedules");
	const groups = usePoll(
		() => (enabled ? listGroups(scope) : Promise.resolve([])),
		0,
	);
	const schedules = usePoll(
		() => (enabled ? listSchedules(scope) : Promise.resolve([])),
		0,
	);
	const refresh = () => {
		groups.reload();
		schedules.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{[groups.error, schedules.error]
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
						id: "schedules",
						label: "Schedules",
						content: (
							<SchedulerSchedules
								scope={scope}
								enabled={enabled}
								groups={groups.data ?? []}
								schedules={schedules.data ?? []}
								loading={schedules.loading}
								resources={resources}
								onChanged={refresh}
							/>
						),
					},
					{
						id: "groups",
						label: "Schedule groups",
						content: (
							<SchedulerGroups
								scope={scope}
								enabled={enabled}
								groups={groups.data ?? []}
								loading={groups.loading}
								onChanged={refresh}
							/>
						),
					},
				]}
			/>
		</SpaceBetween>
	);
}
