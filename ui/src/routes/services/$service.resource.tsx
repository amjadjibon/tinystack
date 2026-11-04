import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	CopyToClipboard,
	Header,
	KeyValuePairs,
	type KeyValuePairsProps,
	SpaceBetween,
	Spinner,
	Table,
	Tabs,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleLayout } from "#/components/Console";
import { EmptyState } from "#/components/EmptyState";
import { GenericResourceActions } from "#/components/GenericResourceActions";
import { ServiceIcon } from "#/components/ServiceIcon";
import { api, type Resource } from "#/lib/api";
import { service } from "#/lib/services";
import { usePoll } from "#/lib/usePoll";

type Search = { type: string; name: string; account: string; region: string };

export const Route = createFileRoute("/services/$service/resource")({
	validateSearch: (search: Record<string, unknown>): Search => ({
		type: String(search.type ?? ""),
		name: String(search.name ?? ""),
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
	}),
	component: ResourcePage,
});

function ResourcePage() {
	const { service: name } = Route.useParams();
	const search = Route.useSearch();
	const { data, loading, reload, error } = usePoll(api.resources);
	const { title } = service(name);
	const resource = data?.find(
		(r) =>
			r.service === name &&
			r.type === search.type &&
			r.name === search.name &&
			r.account === search.account &&
			r.region === search.region,
	);

	return (
		<ConsoleLayout
			contentType="default"
			breadcrumbs={[
				{ text: title, href: `/services/${name}` },
				{ text: search.name, href: "#" },
			]}
			content={
				<ContentLayout
					header={
						<Header
							variant="h1"
							description={`${title} ${search.type}`}
							actions={
								<SpaceBetween direction="horizontal" size="xs">
									{resource && (
										<GenericResourceActions
											resource={resource}
											onChanged={reload}
										/>
									)}
									<Button
										iconName="refresh"
										ariaLabel="Refresh"
										onClick={reload}
									/>
								</SpaceBetween>
							}
						>
							<span
								style={{
									display: "inline-flex",
									alignItems: "center",
									gap: 12,
								}}
							>
								<ServiceIcon name={name} size="large" />
								{search.name}
							</span>
						</Header>
					}
				>
					{!!error && (
						<Alert
							type="error"
							header={
								error.includes("403")
									? "Administrator access required"
									: "Cannot load resources"
							}
						>
							{error}
						</Alert>
					)}
					{resource ? (
						<ResourceDetail resource={resource} />
					) : (
						<Container>
							{loading ? (
								<Spinner size="large" />
							) : (
								<EmptyState
									title="Resource not found"
									subtitle={`${search.type} ${search.name} does not exist in ${search.account} ${search.region}.`}
								/>
							)}
						</Container>
					)}
				</ContentLayout>
			}
		/>
	);
}

function value(v: unknown) {
	if (v === null || v === undefined || v === "") return "-";
	if (typeof v === "object")
		return <CodeBlock text={JSON.stringify(v, null, 2)} />;
	return String(v);
}

function ResourceDetail({ resource }: { resource: Resource }) {
	const details = resource.details ?? {};
	const scalars = Object.entries(details).filter(
		([, v]) => typeof v !== "object" || v === null,
	);
	const objects = Object.entries(details).filter(
		([, v]) => typeof v === "object" && v !== null,
	);
	const tags = Object.entries(resource.tags ?? {}).map(([key, v]) => ({
		key,
		value: v,
	}));

	const summary: KeyValuePairsProps.Item[] = [
		{ label: "Name", value: resource.name },
		{ label: "Type", value: resource.type },
		{ label: "Region", value: resource.region || "global" },
		{ label: "Account", value: resource.account },
		{
			label: "ARN",
			value: resource.arn ? (
				<CopyToClipboard
					variant="inline"
					textToCopy={resource.arn}
					copySuccessText="ARN copied"
					copyErrorText="ARN failed to copy"
				/>
			) : (
				"-"
			),
		},
		{ label: "Tags", value: tags.length },
	];

	return (
		<SpaceBetween size="l">
			<Container header={<Header variant="h2">Summary</Header>}>
				<KeyValuePairs columns={3} items={summary} />
			</Container>
			<Tabs
				tabs={[
					{
						id: "details",
						label: "Details",
						content: (
							<SpaceBetween size="l">
								<Container header={<Header variant="h2">Configuration</Header>}>
									{scalars.length ? (
										<KeyValuePairs
											columns={3}
											items={scalars.map(([label, v]) => ({
												label,
												value: value(v),
											}))}
										/>
									) : (
										<Box color="text-status-inactive">
											No configuration reported.
										</Box>
									)}
								</Container>
								{objects.map(([label, v]) => (
									<Container
										key={label}
										header={<Header variant="h2">{label}</Header>}
									>
										{value(v)}
									</Container>
								))}
							</SpaceBetween>
						),
					},
					{
						id: "tags",
						label: `Tags (${tags.length})`,
						content: (
							<Table
								header={<Header counter={`(${tags.length})`}>Tags</Header>}
								items={tags}
								trackBy="key"
								columnDefinitions={[
									{ id: "key", header: "Key", cell: (t) => t.key },
									{ id: "value", header: "Value", cell: (t) => t.value || "-" },
								]}
								empty={
									<EmptyState
										title="No tags"
										subtitle="This resource has no tags."
									/>
								}
							/>
						),
					},
					{
						id: "json",
						label: "JSON",
						content: (
							<Container header={<Header variant="h2">Resource JSON</Header>}>
								<CodeBlock text={JSON.stringify(resource, null, 2)} />
							</Container>
						),
					},
				]}
			/>
		</SpaceBetween>
	);
}
