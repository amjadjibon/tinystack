import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Header,
	Pagination,
	SpaceBetween,
	Table,
	Toggle,
} from "@cloudscape-design/components";
import { useState } from "react";
import { type CfnScope, cfnPages } from "#/lib/cloudformation";
import {
	cfnDate,
	failed,
	inProgress,
	type StackEvent,
} from "#/lib/cloudformationHelpers";
import { usePoll } from "#/lib/usePoll";
import { StackStatus } from "./StackStatus";

const maxPages = 10;

// StackEvents lists the newest events first and reloads every few seconds
// while the stack operation runs.
export function StackEvents({
	scope,
	status,
}: {
	scope: CfnScope;
	status: string;
}) {
	const running = inProgress(status);
	const poll = usePoll(
		() =>
			cfnPages<StackEvent>(
				scope,
				"DescribeStackEvents",
				"StackEvents",
				{ StackName: scope.stack },
				maxPages,
			),
		running ? 3000 : 0,
	);
	const events = poll.data ?? [];
	const [failedOnly, setFailedOnly] = useState(false);
	const shown = failedOnly
		? events.filter((e) => failed(e.ResourceStatus))
		: events;
	const { items, collectionProps, paginationProps } = useCollection(shown, {
		pagination: { pageSize: 50 },
	});
	return (
		<SpaceBetween size="s">
			{!!poll.error && (
				<Alert type="error" header="Cannot load events">
					{poll.error}
				</Alert>
			)}
			<Table
				{...collectionProps}
				items={items}
				trackBy="EventId"
				loading={poll.loading}
				loadingText="Loading events"
				variant="embedded"
				header={
					<Header
						counter={`(${shown.length})`}
						description={
							running
								? "Following the operation: events reload every 3 seconds."
								: events.length >= maxPages * 100
									? `Showing the newest ${maxPages * 100} events.`
									: undefined
						}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Toggle
									checked={failedOnly}
									onChange={({ detail }) => setFailedOnly(detail.checked)}
								>
									Failed events only
								</Toggle>
								<Button
									iconName="refresh"
									ariaLabel="Refresh events"
									onClick={poll.reload}
								/>
							</SpaceBetween>
						}
					>
						Events
					</Header>
				}
				pagination={<Pagination {...paginationProps} />}
				empty={
					<Box textAlign="center" padding="l">
						{failedOnly ? "No failed events." : "No events for this stack."}
					</Box>
				}
				columnDefinitions={[
					{
						id: "time",
						header: "Time",
						cell: (e) => cfnDate(e.Timestamp),
					},
					{
						id: "logical",
						header: "Logical ID",
						isRowHeader: true,
						cell: (e) => e.LogicalResourceId ?? "—",
					},
					{
						id: "status",
						header: "Status",
						cell: (e) => <StackStatus status={e.ResourceStatus} />,
					},
					{
						id: "type",
						header: "Type",
						cell: (e) => e.ResourceType ?? "—",
					},
					{
						id: "reason",
						header: "Status reason",
						cell: (e) =>
							e.ResourceStatusReason ? (
								failed(e.ResourceStatus) ? (
									<Box color="text-status-error">
										<strong>{e.ResourceStatusReason}</strong>
									</Box>
								) : (
									e.ResourceStatusReason
								)
							) : (
								"—"
							),
					},
				]}
			/>
		</SpaceBetween>
	);
}
