import {
	Badge,
	Box,
	ColumnLayout,
	Container,
	ContentLayout,
	Grid,
	Header,
	KeyValuePairs,
	Link,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { RequestStatus } from "#/components/RequestStatus";
import { ServiceIcon } from "#/components/ServiceIcon";
import { api, type Resource } from "#/lib/api";
import { useHealth } from "#/lib/health";
import { categories, service } from "#/lib/services";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/")({ component: Home });

function Home() {
	const health = useHealth();
	const resources = usePoll(api.resources);
	const requests = usePoll(api.requests);

	return (
		<ConsoleLayout
			contentType="dashboard"
			breadcrumbs={[{ text: "Console Home", href: "/" }]}
			content={
				<ContentLayout
					header={
						<Header
							variant="h1"
							description="A local AWS endpoint for development and tests."
						>
							Console Home
						</Header>
					}
				>
					<Grid
						gridDefinition={[
							{ colspan: 12 },
							{ colspan: { default: 12, l: 8 } },
							{ colspan: { default: 12, l: 4 } },
						]}
					>
						<Container header={<Header variant="h2">Overview</Header>}>
							<KeyValuePairs
								columns={4}
								items={[
									{
										label: "Status",
										value: health.data ? (
											<StatusIndicator>Running</StatusIndicator>
										) : (
											<StatusIndicator
												type={health.error ? "error" : "loading"}
											>
												{health.error ? "Unreachable" : "Connecting"}
											</StatusIndicator>
										),
									},
									{ label: "Storage", value: health.data?.storage ?? "-" },
									{
										label: "Services",
										value: health.data?.services.length ?? "-",
									},
									{ label: "Resources", value: resources.data?.length ?? "-" },
								]}
							/>
						</Container>
						<Services
							names={health.data?.services ?? []}
							resources={resources.data ?? []}
						/>
						<RecentRequests requests={requests.data ?? []} />
					</Grid>
				</ContentLayout>
			}
		/>
	);
}

function Services({
	names,
	resources,
}: {
	names: string[];
	resources: Resource[];
}) {
	const follow = useFollow();
	const counts = new Map<string, number>();
	for (const r of resources)
		counts.set(r.service, (counts.get(r.service) ?? 0) + 1);

	const groups = new Map<string, string[]>();
	for (const name of names) {
		const { category } = service(name);
		groups.set(category, [...(groups.get(category) ?? []), name]);
	}

	const order = (c: string) =>
		categories.includes(c) ? categories.indexOf(c) : categories.length;
	const sorted = [...groups.entries()].sort(([a], [b]) => order(a) - order(b));

	return (
		<Container
			header={
				<Header
					variant="h2"
					counter={`(${names.length})`}
					description="Every service this TinyStack serves."
				>
					Services
				</Header>
			}
		>
			<ColumnLayout columns={3} variant="text-grid">
				{sorted.map(([category, members]) => (
					<SpaceBetween key={category} size="xs">
						<Box variant="h3">{category}</Box>
						{members
							.sort((a, b) => service(a).title.localeCompare(service(b).title))
							.map((name) => (
								<span
									key={name}
									style={{ display: "flex", alignItems: "center", gap: 8 }}
								>
									<ServiceIcon name={name} />
									<Link href={`/services/${name}`} onFollow={follow}>
										{service(name).title}
									</Link>
									{counts.get(name) ? (
										<Badge color="blue">{counts.get(name)}</Badge>
									) : null}
								</span>
							))}
					</SpaceBetween>
				))}
			</ColumnLayout>
		</Container>
	);
}

function RecentRequests({
	requests,
}: {
	requests: Awaited<ReturnType<typeof api.requests>>;
}) {
	const follow = useFollow();
	const recent = requests.slice(0, 8);

	return (
		<Container
			header={<Header variant="h2">Recent requests</Header>}
			footer={
				<Box textAlign="center">
					<Link href="/requests" onFollow={follow} variant="primary">
						View all requests
					</Link>
				</Box>
			}
		>
			{recent.length === 0 ? (
				<Box color="text-status-inactive">No requests yet.</Box>
			) : (
				<SpaceBetween size="s">
					{recent.map((r) => (
						<div
							key={r.request_id}
							style={{ display: "flex", alignItems: "center", gap: 8 }}
						>
							{r.service && <ServiceIcon name={r.service} size="small" />}
							<Box variant="span" fontWeight="bold">
								{r.operation || `${r.method} ${r.path}`}
							</Box>
							<span style={{ marginLeft: "auto" }}>
								<RequestStatus status={r.status} />
							</span>
						</div>
					))}
				</SpaceBetween>
			)}
		</Container>
	);
}
