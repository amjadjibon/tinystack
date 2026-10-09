import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	Flashbar,
	FormField,
	Header,
	Input,
	KeyValuePairs,
	Link,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	Tabs,
	Textarea,
	TextFilter,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleLayout } from "#/components/Console";
import { ConsoleTags } from "#/components/ConsoleTags";
import { EventDeliveries } from "#/components/eventbridge/EventDeliveries";
import { EventPublish } from "#/components/eventbridge/EventPublish";
import { EventRuleDetails } from "#/components/eventbridge/EventRuleDetails";
import { EventRuleList } from "#/components/eventbridge/EventRuleList";
import type { Resource } from "#/lib/api";
import { stringMap } from "#/lib/consoleActions";
import {
	type EventBus,
	type EventScope,
	eventBusArn,
	eventCall,
	eventHref,
	inventoryRule,
} from "#/lib/eventbridge";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/eventbridge")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		bus: String(s.bus ?? ""),
		rule: String(s.rule ?? ""),
		section: s.section === "rules" ? "rules" : "buses",
	}),
	component: EventPage,
});
function EventPage() {
	const search = Route.useSearch(),
		context = useScope();
	const [notice, setNotice] = useState({ scope: "", message: "" });
	const scopeId = `${context.account}/${context.region}`;
	const enabled =
		!!context.capabilities.data?.eventbridge &&
		!context.capabilities.data.requires_signature &&
		/^\d{12}$/.test(context.account) &&
		validRegion(context.region);
	return (
		<EventWorkspace
			key={`${context.account}/${context.region}/${search.bus}/${search.rule}/${search.section}/${enabled}`}
			scope={{
				account: context.account,
				region: context.region,
				bus: search.bus,
				rule: search.rule,
			}}
			section={search.section}
			enabled={enabled}
			notice={notice.scope === scopeId ? notice.message : ""}
			setNotice={(message) => setNotice({ scope: scopeId, message })}
		/>
	);
}
function EventWorkspace({
	scope,
	section,
	enabled,
	notice,
	setNotice,
}: {
	scope: EventScope;
	section: string;
	enabled: boolean;
	notice: string;
	setNotice: (message: string) => void;
}) {
	const navigate = useNavigate(),
		{ inventory, capabilities } = useScope(),
		follow = useFollow();
	const [dialog, setDialog] = useState<"create" | "publish" | null>(null),
		[name, setName] = useState(""),
		[description, setDescription] = useState(""),
		[tags, setTags] = useState("{}");
	const mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	const selected = { ...scope, bus: scope.bus || "default" };
	const go = (bus = "", rule = "", next = section) => {
		if (mounted.current)
			navigate({ href: eventHref({ ...scope, bus, rule }, next) });
	};
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current) setNotice(message);
	};
	const resources = inventory.data ?? [];
	const buses: Resource[] = [
		{
			service: "eventbridge",
			type: "event-bus",
			account: scope.account,
			region: scope.region,
			name: "default",
			arn: eventBusArn({ ...scope, bus: "default" }),
		},
		...resources.filter(
			(r) =>
				r.service === "eventbridge" &&
				r.type === "event-bus" &&
				r.account === scope.account &&
				r.region === scope.region,
		),
	];
	const collection = useCollection(buses, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	const list = eventHref({ ...scope, bus: "", rule: "" }),
		rulesHref = eventHref({ ...scope, bus: "", rule: "" }, "rules");
	return (
		<ConsoleLayout
			navigationHeader={{ text: "Amazon EventBridge", href: list }}
			activeHref={section === "rules" || scope.rule ? rulesHref : list}
			navigationItems={[
				{ type: "link", text: "Event buses", href: list },
				{ type: "link", text: "Rules", href: rulesHref },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Amazon EventBridge", href: list },
				{
					text: section === "rules" ? "Rules" : "Event buses",
					href: section === "rules" ? rulesHref : list,
				},
				...(scope.bus
					? [{ text: scope.bus, href: eventHref({ ...scope, rule: "" }) }]
					: []),
				...(scope.rule
					? [{ text: scope.rule, href: eventHref(scope, "rules") }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{notice && (
						<Flashbar
							items={[
								{
									id: "eventbridge-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error">{inventory.error || capabilities.error}</Alert>
					)}
					{capabilities.data?.requires_signature && (
						<Alert type="info">
							EventBridge inventory and deliveries are available here. Use
							signed CLI/SDK requests for management and event publication.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.eventbridge && (
						<Alert type="warning">
							EventBridge is not enabled on this server.
						</Alert>
					)}
					{scope.rule ? (
						<EventRuleDetails
							scope={selected}
							resource={resources.find(
								(r) =>
									r.service === "eventbridge" &&
									r.type === "rule" &&
									r.account === scope.account &&
									r.region === scope.region &&
									r.name === scope.rule &&
									inventoryRule(r).EventBusName === selected.bus,
							)}
							resources={resources}
							enabled={enabled}
							onChanged={changed}
							onDeleted={() => go(selected.bus, "", "rules")}
						/>
					) : section === "rules" ? (
						<ContentLayout
							header={
								<Header
									variant="h1"
									actions={
										<Button
											disabled={!enabled}
											onClick={() => setDialog("publish")}
										>
											Send event
										</Button>
									}
								>
									Rules
								</Header>
							}
						>
							<SpaceBetween size="l">
								<FormField label="Event bus">
									<Select
										selectedOption={{
											value: selected.bus,
											label: selected.bus,
										}}
										options={buses.map((b) => ({
											value: b.name,
											label: b.name,
										}))}
										onChange={({ detail }) =>
											go(detail.selectedOption.value ?? "default", "", "rules")
										}
									/>
								</FormField>
								<EventRuleList
									scope={selected}
									resources={resources}
									loading={inventory.loading}
									enabled={enabled}
									onChanged={changed}
									onOpen={(name) => go(selected.bus, name, "rules")}
								/>
							</SpaceBetween>
						</ContentLayout>
					) : scope.bus ? (
						<EventBusDetails
							scope={selected}
							resource={buses.find((b) => b.name === scope.bus)}
							resources={resources}
							enabled={enabled}
							onChanged={changed}
							onDeleted={() => go()}
							onOpen={(name) => go(selected.bus, name, "rules")}
							onPublish={() => setDialog("publish")}
						/>
					) : (
						<ContentLayout header={<Header variant="h1">Event buses</Header>}>
							<Table
								{...collection.collectionProps}
								items={collection.items}
								loading={inventory.loading}
								trackBy="arn"
								header={
									<Header
										counter={`(${buses.length})`}
										description="Route events in the selected account and Region."
										actions={
											<SpaceBetween direction="horizontal" size="xs">
												<Button
													iconName="refresh"
													ariaLabel="Refresh event buses"
													onClick={inventory.reload}
												/>
												<Button
													variant="primary"
													disabled={!enabled}
													onClick={() => setDialog("create")}
												>
													Create event bus
												</Button>
											</SpaceBetween>
										}
									>
										Event buses
									</Header>
								}
								filter={
									<TextFilter
										{...collection.filterProps}
										filteringPlaceholder="Find event buses"
									/>
								}
								pagination={<Pagination {...collection.paginationProps} />}
								columnDefinitions={[
									{
										id: "name",
										header: "Name",
										sortingField: "name",
										cell: (b) => (
											<Link
												href={eventHref({ ...scope, bus: b.name })}
												onFollow={follow}
											>
												{b.name}
											</Link>
										),
									},
									{
										id: "description",
										header: "Description",
										cell: (b) => String(b.details?.Description ?? "—"),
									},
									{ id: "arn", header: "ARN", cell: (b) => b.arn },
								]}
								empty={
									<Box textAlign="center" padding="l">
										No event buses match your filter.
									</Box>
								}
							/>
						</ContentLayout>
					)}
					{dialog === "publish" && (
						<EventPublish scope={selected} onClose={() => setDialog(null)} />
					)}
					{dialog === "create" && (
						<ActionDialog
							title="Create event bus"
							submit="Create event bus"
							onClose={() => setDialog(null)}
							onSaved={() => {
								setDialog(null);
								changed(`Event bus ${name} created.`);
								go(name);
							}}
							plan={async () => {
								if (
									!/^[A-Za-z0-9_.-]{1,256}$/.test(name) ||
									name === "default" ||
									name.startsWith("aws.")
								)
									throw new Error(
										"Enter a custom bus name using 1–256 letters, numbers, underscores, hyphens or periods. The names default and aws.* are reserved.",
									);
								const values = stringMap(tags, "Tags");
								return [
									{
										label: "Create event bus",
										run: () =>
											eventCall(scope, "CreateEventBus", {
												Name: name,
												Description: description,
												Tags: Object.entries(values).map(([Key, Value]) => ({
													Key,
													Value,
												})),
											}),
									},
								];
							}}
						>
							<SpaceBetween size="l">
								<FormField label="Event bus name">
									<Input
										ariaLabel="Event bus name"
										value={name}
										onChange={({ detail }) => setName(detail.value)}
									/>
								</FormField>
								<FormField label="Description — optional">
									<Input
										ariaLabel="Event bus description"
										value={description}
										onChange={({ detail }) => setDescription(detail.value)}
									/>
								</FormField>
								<FormField label="Tags (JSON) — optional">
									<Textarea
										ariaLabel="Event bus tags"
										value={tags}
										onChange={({ detail }) => setTags(detail.value)}
									/>
								</FormField>
							</SpaceBetween>
						</ActionDialog>
					)}
				</SpaceBetween>
			}
		/>
	);
}
function EventBusDetails({
	scope,
	resource,
	resources,
	enabled,
	onChanged,
	onDeleted,
	onOpen,
	onPublish,
}: {
	scope: EventScope;
	resource?: Resource;
	resources: Resource[];
	enabled: boolean;
	onChanged: (message: string) => void;
	onDeleted: () => void;
	onOpen: (name: string) => void;
	onPublish: () => void;
}) {
	const bus = usePoll(async () =>
		enabled
			? await eventCall<EventBus>(scope, "DescribeEventBus", {
					Name: scope.bus,
				})
			: ((resource?.details as EventBus) ?? {
					Name: scope.bus,
					Arn: eventBusArn(scope),
				}),
	);
	const tags = usePoll(async () =>
		enabled && scope.bus !== "default"
			? Object.fromEntries(
					(
						(
							await eventCall<{ Tags?: { Key: string; Value: string }[] }>(
								scope,
								"ListTagsForResource",
								{ ResourceARN: eventBusArn(scope) },
							)
						).Tags ?? []
					).map((t) => [t.Key, t.Value]),
				)
			: (resource?.tags ?? {}),
	);
	const data = bus.data ?? {},
		[dialog, setDialog] = useState<"edit" | "delete" | null>(null),
		[description, setDescription] = useState("");
	const refresh = (message: string) => {
		bus.reload();
		tags.reload();
		onChanged(message);
	};
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button disabled={!enabled || !data.Name} onClick={onPublish}>
								Send event
							</Button>
							<Button
								disabled={!enabled || scope.bus === "default" || !data.Name}
								onClick={() => {
									setDescription(data.Description ?? "");
									setDialog("edit");
								}}
							>
								Edit
							</Button>
							<Button
								disabled={!enabled || scope.bus === "default" || !data.Name}
								onClick={() => setDialog("delete")}
							>
								Delete
							</Button>
						</SpaceBetween>
					}
				>
					{scope.bus}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{(bus.error || tags.error) && (
					<Alert type="error">{bus.error || tags.error}</Alert>
				)}
				<Container header={<Header variant="h2">Event bus details</Header>}>
					<KeyValuePairs
						columns={2}
						items={[
							{ label: "ARN", value: data.Arn || eventBusArn(scope) },
							{ label: "Description", value: data.Description || "—" },
							{
								label: "Created",
								value: data.CreationTime
									? new Date(data.CreationTime * 1000).toLocaleString()
									: "—",
							},
						]}
					/>
				</Container>
				<Tabs
					tabs={[
						{
							id: "rules",
							label: "Rules",
							content: (
								<EventRuleList
									scope={scope}
									resources={resources}
									loading={bus.loading}
									enabled={enabled && !!data.Name}
									onChanged={refresh}
									onOpen={onOpen}
								/>
							),
						},
						{
							id: "tags",
							label: "Tags",
							content: (
								<ConsoleTags
									tags={tags.data ?? resource?.tags ?? {}}
									enabled={enabled && scope.bus !== "default" && !!tags.data}
									onChanged={() => refresh("Event bus tags updated.")}
									plan={(next, removed) => [
										...(removed.length
											? [
													{
														label: "Remove tags",
														run: () =>
															eventCall(scope, "UntagResource", {
																ResourceARN: eventBusArn(scope),
																TagKeys: removed,
															}),
													},
												]
											: []),
										...(Object.keys(next).length
											? [
													{
														label: "Save tags",
														run: () =>
															eventCall(scope, "TagResource", {
																ResourceARN: eventBusArn(scope),
																Tags: Object.entries(next).map(
																	([Key, Value]) => ({ Key, Value }),
																),
															}),
													},
												]
											: []),
									]}
								/>
							),
						},
						{
							id: "policy",
							label: "Permissions",
							content: (
								<SpaceBetween size="l">
									<Alert type="info">
										Manage bus permissions through signed AWS API requests.
										TinyStack stores these policies without enforcing
										cross-account event access.
									</Alert>
									<CodeBlock
										text={
											data.Policy ||
											(!enabled && scope.bus === "default"
												? "Read the default bus policy using a signed API request."
												: "No permissions policy configured.")
										}
									/>
								</SpaceBetween>
							),
						},
						{
							id: "deliveries",
							label: "Deliveries",
							content: <EventDeliveries scope={scope} />,
						},
					]}
				/>
				{dialog && (
					<ActionDialog
						title={dialog === "delete" ? "Delete event bus" : "Edit event bus"}
						submit={dialog === "delete" ? "Delete event bus" : "Save changes"}
						confirmation={dialog === "delete" ? scope.bus : undefined}
						onClose={() => setDialog(null)}
						onSaved={() => {
							setDialog(null);
							if (dialog === "delete") onDeleted();
							else refresh("Event bus updated.");
						}}
						plan={async () => [
							{
								label:
									dialog === "delete" ? "Delete event bus" : "Update event bus",
								run: () =>
									eventCall(
										scope,
										dialog === "delete" ? "DeleteEventBus" : "UpdateEventBus",
										{
											Name: scope.bus,
											...(dialog === "edit"
												? { Description: description }
												: {}),
										},
									),
							},
						]}
					>
						{dialog === "delete" ? (
							<Alert type="warning">
								Delete {scope.bus}. Remove its rules first. The default event
								bus cannot be deleted.
							</Alert>
						) : (
							<FormField label="Description">
								<Input
									ariaLabel="Event bus description"
									value={description}
									onChange={({ detail }) => setDescription(detail.value)}
								/>
							</FormField>
						)}
					</ActionDialog>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}
