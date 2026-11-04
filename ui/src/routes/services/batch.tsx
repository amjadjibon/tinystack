import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { Definitions } from "#/components/batch/Definitions";
import { Environments } from "#/components/batch/Environments";
import { Jobs } from "#/components/batch/Jobs";
import { Queues } from "#/components/batch/Queues";
import { ConsoleLayout } from "#/components/Console";
import {
	allStatuses,
	type BatchScope,
	type BatchSection,
	batchHref,
	batchSections,
	definitionStatuses,
	jobStatuses,
	sectionTitles,
} from "#/lib/batchHelpers";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";

export const Route = createFileRoute("/services/batch")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		section: batchSections.find((x) => x === s.section) ?? "environments",
		environment: String(s.environment ?? ""),
		queue: String(s.queue ?? ""),
		definition: String(s.definition ?? ""),
		job: String(s.job ?? ""),
		status: String(s.status ?? ""),
		tab: s.tab === "tags" ? "tags" : "overview",
	}),
	component: BatchPage,
});

type Search = {
	environment: string;
	queue: string;
	definition: string;
	job: string;
	status: string;
	tab: string;
};

function BatchPage() {
	const { account, region, canWrite, capabilities, inventory } = useScope();
	const search = Route.useSearch();
	const enabled =
		!!capabilities.data?.pages.batch &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const docker = capabilities.data?.batch_runtime === "docker";
	const section = search.section;
	// The resource whose page is open, and the address parameter that names it.
	// A jobs list also names its queue, but only a job opens a page.
	const { param, open } = {
		environments: { param: "environment", open: search.environment },
		queues: { param: "queue", open: search.queue },
		definitions: { param: "definition", open: search.definition },
		jobs: { param: "job", open: search.job },
	}[section];
	const home = batchHref(scope);
	return (
		<ConsoleLayout
			contentType={open ? "default" : "table"}
			navigationHeader={{ text: "AWS Batch", href: home }}
			activeHref={batchHref(scope, section)}
			navigationItems={[
				...batchSections.map((s) => ({
					type: "link" as const,
					text: sectionTitles[s],
					href: batchHref(scope, s),
				})),
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "AWS Batch", href: home },
				{ text: sectionTitles[section], href: batchHref(scope, section) },
				...(open
					? [{ text: open, href: batchHref(scope, section, { [param]: open }) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.batch && (
						<Alert type="warning">
							AWS Batch is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert>
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<Alert
						header={
							docker ? "Docker runtime configured" : "Records-only runtime"
						}
					>
						{docker
							? "Fargate submissions are explicitly rejected in Docker mode. Each EC2 job attempt runs in a local Docker container. A queue starts its jobs by priority, in each compute environment's order, while the environment has vCPUs to spare, and the page refreshes while jobs are in progress. Container output goes to CloudWatch Logs. No instances are provisioned: compute environments are metadata."
							: "Compute environments, job queues, job definitions and jobs are stored as records. Environments and queues become VALID immediately because no infrastructure is provisioned. No container runs, so a submitted job becomes RUNNABLE (PENDING while it waits for another job) and stays there; it never starts, succeeds or fails on its own. Cancel or terminate it to fail it. Set batch.jobs to docker to run EC2 jobs in containers."}
					</Alert>
					<Section
						key={`${account}/${region}/${enabled}/${section}`}
						scope={scope}
						section={section}
						enabled={enabled}
						docker={docker}
						search={search}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}

function Section({
	scope,
	section,
	enabled,
	docker,
	search,
	onChanged,
}: {
	scope: BatchScope;
	section: BatchSection;
	enabled: boolean;
	docker: boolean;
	search: Search;
	onChanged: () => void;
}) {
	const common = { scope, enabled, docker, tab: search.tab, onChanged };
	switch (section) {
		case "environments":
			return <Environments {...common} selected={search.environment} />;
		case "queues":
			return <Queues {...common} selected={search.queue} />;
		case "definitions": {
			const status = definitionStatuses.includes(search.status)
				? search.status
				: "ACTIVE";
			return (
				<Definitions
					key={`${status}/${search.definition}`}
					{...common}
					selected={search.definition}
					status={status}
				/>
			);
		}
		case "jobs":
			return (
				<Jobs
					{...common}
					selected={search.job}
					queue={search.queue}
					status={
						[allStatuses, ...jobStatuses].includes(search.status)
							? search.status
							: allStatuses
					}
				/>
			);
	}
}
