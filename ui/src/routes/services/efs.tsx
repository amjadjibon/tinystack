import { Alert, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { AccessPoints } from "#/components/efs/AccessPoints";
import { FileSystemDetails } from "#/components/efs/FileSystemDetails";
import { FileSystems } from "#/components/efs/FileSystems";
import { type EFSScope, type EFSTab, efsHref, efsTabs } from "#/lib/efs";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/efs")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		filesystem: String(search.filesystem ?? ""),
		tab: efsTabs.find((t) => t === search.tab) ?? "filesystems",
	}),
	component: EFSPage,
});

function EFSPage() {
	const search = Route.useSearch();
	const { canWrite, account, region, capabilities, inventory } = useScope();
	const enabled =
		!!capabilities.data?.efs &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = efsHref(scope);
	const accessPoints = efsHref({ ...scope, tab: "accesspoints" });
	return (
		<ConsoleLayout
			contentType={search.filesystem ? "default" : "table"}
			navigationHeader={{ text: "Elastic File System", href: home }}
			activeHref={
				!search.filesystem && search.tab === "accesspoints"
					? accessPoints
					: home
			}
			navigationItems={[
				{ type: "link", text: "File systems", href: home },
				{ type: "link", text: "Access points", href: accessPoints },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Elastic File System", href: home },
				...(search.filesystem
					? [
							{
								text: search.filesystem,
								href: efsHref({ ...scope, filesystem: search.filesystem }),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.efs && (
						<Alert type="warning">
							Elastic File System is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert type="info">
							Connect a console session for this account to manage file systems.
						</Alert>
					)}
					<Workspace
						key={`${account}/${region}/${enabled}`}
						scope={scope}
						enabled={enabled}
						filesystem={search.filesystem}
						tab={search.tab}
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
	filesystem,
	tab,
	onChanged,
}: {
	scope: EFSScope;
	enabled: boolean;
	filesystem: string;
	tab: EFSTab;
	onChanged: () => void;
}) {
	const navigate = useNavigate();
	if (filesystem)
		return (
			<FileSystemDetails
				key={filesystem}
				scope={scope}
				id={filesystem}
				enabled={enabled}
				onChanged={onChanged}
				onBack={() => navigate({ href: efsHref(scope) })}
			/>
		);
	return (
		<Tabs
			activeTabId={tab}
			onChange={({ detail }) =>
				navigate({
					href: efsHref({ ...scope, tab: detail.activeTabId as EFSTab }),
				})
			}
			tabs={[
				{
					id: "filesystems",
					label: "File systems",
					content:
						tab === "filesystems" ? (
							<FileSystems
								scope={scope}
								enabled={enabled}
								onCreated={(id) => {
									onChanged();
									navigate({ href: efsHref({ ...scope, filesystem: id }) });
								}}
							/>
						) : null,
				},
				{
					id: "accesspoints",
					label: "Access points",
					content:
						tab === "accesspoints" ? (
							<AccessPoints
								scope={scope}
								enabled={enabled}
								onChanged={onChanged}
							/>
						) : null,
				},
			]}
		/>
	);
}
