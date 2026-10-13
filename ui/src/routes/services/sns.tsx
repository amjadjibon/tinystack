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
import { SNSSubscription } from "#/components/sns/SNSSubscription";
import { SNSSubscriptions } from "#/components/sns/SNSSubscriptions";
import { SNSTopic } from "#/components/sns/SNSTopic";
import { SNSTopicForm } from "#/components/sns/SNSTopicForm";
import type { Resource } from "#/lib/api";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { type SNSScope, snsHref, topicSubscriptions } from "#/lib/sns";
import { useFollow } from "#/lib/useFollow";

export const Route = createFileRoute("/services/sns")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		topic: String(search.topic ?? ""),
		subscription: String(search.subscription ?? ""),
		section: search.section === "subscriptions" ? "subscriptions" : "topics",
		create: search.create === true || search.create === "true",
	}),
	component: SNSPage,
});
function SNSPage() {
	const search = Route.useSearch();
	const navigate = useNavigate();
	const location = useLocation();
	const { canWrite, account, region, inventory, capabilities } = useScope();
	const scope = { account, region, topic: search.topic };
	const [notice, setNotice] = useState("");
	const context = `${account}/${region}/${search.topic}/${search.subscription}/${search.create}/${search.section}/${location.pathname}`;
	const current = useRef(context);
	current.current = context;
	const mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	// biome-ignore lint/correctness/useExhaustiveDependencies: notices belong to the selected account and Region.
	useEffect(() => setNotice(""), [account, region]);
	const enabled =
		!!capabilities.data?.sns &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const topics = (inventory.data ?? []).filter(
		(r) =>
			r.service === "sns" &&
			r.type === "topic" &&
			r.account === account &&
			r.region === region,
	);
	const topic = topics.find((t) => t.name === search.topic);
	const subscriptions = topics.flatMap(topicSubscriptions);
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current && current.current === context) setNotice(message);
	};
	const go = (
		topic = "",
		subscription = "",
		section = "topics",
		create = false,
	) => {
		if (!mounted.current || current.current !== context) return;
		return navigate({
			to: "/services/sns",
			search: { account, region, topic, subscription, section, create },
		});
	};
	const topicsHref = snsHref({ account, region });
	const subscriptionsHref = snsHref({ account, region }, "", "subscriptions");
	return (
		<ConsoleLayout
			contentType={search.topic || search.create ? "default" : "table"}
			navigationHeader={{ text: "Amazon SNS", href: topicsHref }}
			activeHref={
				search.section === "subscriptions" ? subscriptionsHref : topicsHref
			}
			navigationItems={[
				{ type: "link", text: "Topics", href: topicsHref },
				{ type: "link", text: "Subscriptions", href: subscriptionsHref },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Amazon SNS", href: topicsHref },
				{
					text: search.section === "subscriptions" ? "Subscriptions" : "Topics",
					href:
						search.section === "subscriptions" ? subscriptionsHref : topicsHref,
				},
				...(search.create
					? [
							{
								text: "Create topic",
								href: snsHref({ account, region }, "", "topics", true),
							},
						]
					: search.topic
						? [{ text: search.topic, href: snsHref(scope) }]
						: []),
				...(search.subscription
					? [
							{
								text: "Subscription",
								href: snsHref(scope, search.subscription),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{notice && (
						<Flashbar
							items={[
								{
									id: "sns-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error" header="Cannot load SNS">
							{inventory.error || capabilities.error}
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info" header="SNS management is read-only">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.sns && (
						<Alert type="warning">SNS is not enabled on this server.</Alert>
					)}
					{search.create ? (
						enabled ? (
							<SNSTopicForm
								key={`${account}/${region}`}
								scope={{ account, region }}
								topics={topics}
								onCancel={() => go()}
								onSaved={(name) => {
									changed(`Topic ${name} created.`);
									go(name);
								}}
							/>
						) : (
							<Box>
								Topic creation is unavailable in the current server mode.
							</Box>
						)
					) : search.topic && search.subscription ? (
						<SNSSubscription
							key={`${account}/${region}/${search.subscription}/${enabled}`}
							scope={scope}
							arn={search.subscription}
							subscription={subscriptions.find(
								(s) => s.SubscriptionArn === search.subscription,
							)}
							resources={inventory.data ?? []}
							fifo={search.topic.endsWith(".fifo")}
							enabled={enabled}
							onChanged={changed}
							onDeleted={() => {
								changed("Subscription deleted.");
								go(search.topic);
							}}
							onConfirmed={(arn) => go(search.topic, arn)}
						/>
					) : search.topic ? (
						<SNSTopic
							key={`${account}/${region}/${search.topic}/${enabled}`}
							scope={scope}
							resource={topic}
							resources={inventory.data ?? []}
							enabled={enabled}
							inventoryLoading={inventory.loading}
							onRefresh={inventory.reload}
							onChanged={changed}
							onDeleted={() => {
								changed(`Topic ${search.topic} deleted.`);
								go();
							}}
						/>
					) : search.section === "subscriptions" ? (
						<ContentLayout
							header={
								<Header
									variant="h1"
									description="Subscriptions in the selected account and Region. Open a topic to create a subscription."
								>
									Subscriptions
								</Header>
							}
						>
							<SNSSubscriptions
								key={`${account}/${region}`}
								subscriptions={subscriptions}
								loading={inventory.loading}
								onRefresh={inventory.reload}
							/>
						</ContentLayout>
					) : (
						<TopicList
							key={`${account}/${region}`}
							scope={scope}
							topics={topics}
							loading={inventory.loading}
							enabled={enabled}
							onRefresh={inventory.reload}
							onCreate={() => go("", "", "topics", true)}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}
function TopicList({
	scope,
	topics,
	loading,
	enabled,
	onRefresh,
	onCreate,
}: {
	scope: SNSScope;
	topics: Resource[];
	loading: boolean;
	enabled: boolean;
	onRefresh: () => void;
	onCreate: () => void;
}) {
	const follow = useFollow();
	const collection = useCollection(topics, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description="Publish messages to subscribers in the selected account and Region."
				>
					Topics
				</Header>
			}
		>
			<Table
				{...collection.collectionProps}
				items={collection.items}
				loading={loading}
				loadingText="Loading topics"
				trackBy="arn"
				header={
					<Header
						counter={`(${topics.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh topics"
									onClick={onRefresh}
								/>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={onCreate}
								>
									Create topic
								</Button>
							</SpaceBetween>
						}
					>
						Topics
					</Header>
				}
				filter={
					<TextFilter
						{...collection.filterProps}
						filteringPlaceholder="Find topics"
						filteringAriaLabel="Find topics"
					/>
				}
				pagination={<Pagination {...collection.paginationProps} />}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						sortingField: "name",
						cell: (t) => (
							<Link
								href={snsHref({ ...scope, topic: t.name })}
								onFollow={follow}
							>
								{t.name}
							</Link>
						),
					},
					{
						id: "type",
						header: "Type",
						cell: (t) =>
							(t.details?.Attributes as Record<string, string>)?.FifoTopic ===
							"true"
								? "FIFO"
								: "Standard",
					},
					{
						id: "display",
						header: "Display name",
						cell: (t) =>
							(t.details?.Attributes as Record<string, string>)?.DisplayName ||
							"—",
					},
					{
						id: "confirmed",
						header: "Confirmed subscriptions",
						cell: (t) =>
							topicSubscriptions(t).filter((s) => !s.PendingConfirmation)
								.length,
					},
					{
						id: "pending",
						header: "Pending subscriptions",
						cell: (t) =>
							topicSubscriptions(t).filter((s) => s.PendingConfirmation).length,
					},
					{ id: "arn", header: "ARN", cell: (t) => t.arn },
				]}
				empty={
					<Box textAlign="center" padding="l">
						<SpaceBetween size="m">
							<Box variant="h3">
								{collection.filterProps.filteringText
									? "No matching topics"
									: "No topics"}
							</Box>
							<Box>
								{collection.filterProps.filteringText
									? "Clear the filter to see all topics."
									: "Create a topic to publish messages."}
							</Box>
						</SpaceBetween>
					</Box>
				}
			/>
		</ContentLayout>
	);
}
