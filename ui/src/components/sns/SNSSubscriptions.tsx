import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Box,
	Button,
	Header,
	Hotspot,
	Link,
	Pagination,
	SpaceBetween,
	StatusIndicator,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { type SNSSubscription, snsHref, subscriptionScope } from "#/lib/sns";
import { useFollow } from "#/lib/useFollow";
import { HOTSPOT } from "#/tutorials/hotspots";

export function SNSSubscriptions({
	subscriptions,
	loading,
	onRefresh,
	onCreate,
}: {
	subscriptions: SNSSubscription[];
	loading: boolean;
	onRefresh: () => void;
	onCreate?: () => void;
}) {
	const follow = useFollow();
	const collection = useCollection(subscriptions, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	return (
		<Table
			{...collection.collectionProps}
			items={collection.items}
			loading={loading}
			loadingText="Loading subscriptions"
			trackBy="SubscriptionArn"
			header={
				<Header
					variant="h2"
					counter={`(${subscriptions.length})`}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh subscriptions"
								onClick={onRefresh}
							/>
							{onCreate && (
								<Hotspot hotspotId={HOTSPOT.snsSubscribe}>
									<Button variant="primary" onClick={onCreate}>
										Create subscription
									</Button>
								</Hotspot>
							)}
						</SpaceBetween>
					}
				>
					Subscriptions
				</Header>
			}
			filter={
				<TextFilter
					{...collection.filterProps}
					filteringPlaceholder="Find subscriptions"
					filteringAriaLabel="Find subscriptions"
				/>
			}
			pagination={<Pagination {...collection.paginationProps} />}
			columnDefinitions={[
				{
					id: "id",
					header: "Subscription ID",
					sortingField: "SubscriptionArn",
					cell: (s) => (
						<Link
							href={snsHref(subscriptionScope(s), s.SubscriptionArn)}
							onFollow={follow}
						>
							{s.SubscriptionArn.split(":").at(-1)}
						</Link>
					),
				},
				{
					id: "protocol",
					header: "Protocol",
					sortingField: "Protocol",
					cell: (s) => s.Protocol,
				},
				{ id: "endpoint", header: "Endpoint", cell: (s) => s.Endpoint },
				{
					id: "status",
					header: "Status",
					cell: (s) => (
						<StatusIndicator
							type={s.PendingConfirmation ? "pending" : "success"}
						>
							{s.PendingConfirmation ? "Pending confirmation" : "Confirmed"}
						</StatusIndicator>
					),
				},
				{
					id: "topic",
					header: "Topic",
					cell: (s) => (
						<Link href={snsHref(subscriptionScope(s))} onFollow={follow}>
							{subscriptionScope(s).topic}
						</Link>
					),
				},
				{
					id: "raw",
					header: "Raw delivery",
					cell: (s) => (s.RawMessageDelivery ? "Enabled" : "Disabled"),
				},
			]}
			empty={
				<Box textAlign="center" padding="l">
					{collection.filterProps.filteringText
						? "No matching subscriptions"
						: "No subscriptions. Open a topic to create one."}
				</Box>
			}
		/>
	);
}
