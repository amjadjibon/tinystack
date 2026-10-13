import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	ContentLayout,
	Flashbar,
	Header,
	Link,
	Pagination,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import {
	createFileRoute,
	useLocation,
	useNavigate,
} from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { SQSQueue } from "#/components/sqs/SQSQueue";
import { SQSQueueForm } from "#/components/sqs/SQSQueueForm";
import type { Resource } from "#/lib/api";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { type SQSScope, sqsHref } from "#/lib/sqs";
import { useFollow } from "#/lib/useFollow";

export const Route = createFileRoute("/services/sqs")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		queue: String(search.queue ?? ""),
		create: search.create === true || search.create === "true",
	}),
	component: SQSPage,
});
function SQSPage() {
	const search = Route.useSearch();
	const navigate = useNavigate();
	const { canWrite, account, region, inventory, capabilities } = useScope();
	const scope = { account, region, queue: search.queue };
	const location = useLocation();
	const context = `${account}/${region}/${search.queue}/${search.create}/${location.pathname}`;
	const current = useRef(context);
	current.current = context;
	const mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	const [notice, setNotice] = useState("");
	// biome-ignore lint/correctness/useExhaustiveDependencies: notices belong to the selected account and Region.
	useEffect(() => setNotice(""), [account, region]);
	const enabled =
		!!capabilities.data?.sqs &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const queues = (inventory.data ?? []).filter(
		(r) =>
			r.service === "sqs" &&
			r.type === "queue" &&
			r.account === account &&
			r.region === region,
	);
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current && current.current === context) setNotice(message);
	};
	const go = (queue = "", create = false) => {
		if (!mounted.current || current.current !== context) return;
		return navigate({
			to: "/services/sqs",
			search: { account, region, queue, create },
		});
	};
	const listHref = sqsHref({ account, region });
	return (
		<ConsoleLayout
			contentType={search.queue || search.create ? "default" : "table"}
			navigationHeader={{ text: "Amazon SQS", href: listHref }}
			activeHref={listHref}
			navigationItems={[
				{ type: "link", text: "Queues", href: listHref },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Amazon SQS", href: listHref },
				{ text: "Queues", href: listHref },
				...(search.create
					? [{ text: "Create queue", href: sqsHref({ account, region }, true) }]
					: search.queue
						? [{ text: search.queue, href: sqsHref(scope) }]
						: []),
			]}
			content={
				<SpaceBetween size="l">
					{notice && (
						<Flashbar
							items={[
								{
									id: "sqs-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error" header="Cannot load SQS">
							{inventory.error || capabilities.error}
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info" header="SQS management is read-only">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.sqs && (
						<Alert type="warning">SQS is not enabled on this server.</Alert>
					)}
					{search.create ? (
						enabled ? (
							<SQSQueueForm
								key={`${account}/${region}`}
								scope={{ account, region }}
								queues={queues}
								onCancel={() => go()}
								onSaved={(name) => {
									changed(`Queue ${name} created.`);
									go(name);
								}}
							/>
						) : (
							<Box>
								Queue creation is unavailable in the current server mode.
							</Box>
						)
					) : search.queue ? (
						<SQSQueue
							key={`${account}/${region}/${search.queue}/${enabled}`}
							scope={scope}
							queues={queues}
							enabled={enabled}
							resource={queues.find((q) => q.name === search.queue)}
							onChanged={changed}
							onDeleted={() => {
								changed(`Queue ${search.queue} deleted.`);
								go();
							}}
						/>
					) : (
						<QueueList
							key={`${account}/${region}`}
							scope={scope}
							queues={queues}
							loading={inventory.loading}
							enabled={enabled}
							onRefresh={inventory.reload}
							onOpen={(name) => go(name)}
							onCreate={() => go("", true)}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}
function QueueList({
	scope,
	queues,
	loading,
	enabled,
	onRefresh,
	onOpen,
	onCreate,
}: {
	scope: SQSScope;
	queues: Resource[];
	loading: boolean;
	enabled: boolean;
	onRefresh: () => void;
	onOpen: (name: string) => void;
	onCreate: () => void;
}) {
	const follow = useFollow();
	const collection = useCollection(queues, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
		selection: { trackBy: "arn" },
	});
	const selected = collection.collectionProps.selectedItems?.[0];
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description="Manage message queues in the selected account and Region."
				>
					Queues
				</Header>
			}
		>
			<Table
				{...collection.collectionProps}
				items={collection.items}
				loading={loading}
				loadingText="Loading queues"
				trackBy="arn"
				selectionType="single"
				selectedItems={collection.collectionProps.selectedItems ?? []}
				ariaLabels={{
					selectionGroupLabel: "Select queue",
					itemSelectionLabel: (_, q) => `Select ${q.name}`,
				}}
				header={
					<Header
						counter={`(${queues.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh queues"
									onClick={onRefresh}
								/>
								<Button
									disabled={!selected}
									onClick={() => selected && onOpen(selected.name)}
								>
									View details
								</Button>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={onCreate}
								>
									Create queue
								</Button>
							</SpaceBetween>
						}
					>
						Queues
					</Header>
				}
				filter={
					<TextFilter
						{...collection.filterProps}
						filteringPlaceholder="Find queues"
						filteringAriaLabel="Find queues"
					/>
				}
				pagination={<Pagination {...collection.paginationProps} />}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						sortingField: "name",
						cell: (q) => (
							<Link
								href={sqsHref({ ...scope, queue: q.name })}
								onFollow={follow}
							>
								{q.name}
							</Link>
						),
					},
					{
						id: "type",
						header: "Type",
						cell: (q) =>
							q.details?.FifoQueue === "true" ? "FIFO" : "Standard",
					},
					{
						id: "available",
						header: "Messages available",
						cell: (q) => String(q.details?.ApproximateNumberOfMessages ?? 0),
					},
					{
						id: "inflight",
						header: "Messages in flight",
						cell: (q) =>
							String(q.details?.ApproximateNumberOfMessagesNotVisible ?? 0),
					},
					{
						id: "delayed",
						header: "Messages delayed",
						cell: (q) =>
							String(q.details?.ApproximateNumberOfMessagesDelayed ?? 0),
					},
					{
						id: "created",
						header: "Created",
						cell: (q) =>
							q.details?.CreatedTimestamp
								? new Date(
										Number(q.details.CreatedTimestamp) * 1000,
									).toLocaleString()
								: "—",
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						<SpaceBetween size="m">
							<Box variant="h3">
								{collection.filterProps.filteringText
									? "No matching queues"
									: "No queues"}
							</Box>
							<Box>
								{collection.filterProps.filteringText
									? "Clear the filter to see all queues."
									: "Create a queue to start sending and receiving messages."}
							</Box>
						</SpaceBetween>
					</Box>
				}
			/>
		</ContentLayout>
	);
}
