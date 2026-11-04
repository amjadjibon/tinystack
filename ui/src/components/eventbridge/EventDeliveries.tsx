import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Header,
	Link,
	Modal,
	Pagination,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { api, type Delivery, pretty } from "#/lib/api";
import { type EventScope, eventRuleArn } from "#/lib/eventbridge";
import { usePoll } from "#/lib/usePoll";
export function EventDeliveries({ scope }: { scope: EventScope }) {
	const poll = usePoll(api.deliveries),
		[selected, setSelected] = useState<Delivery>();
	const rulePrefix = eventRuleArn({ ...scope, rule: "" });
	const rows = (poll.data ?? []).filter(
		(d) =>
			d.via === "eventbridge" &&
			(scope.rule
				? d.source === eventRuleArn(scope)
				: d.source?.startsWith(rulePrefix) &&
					(scope.bus !== "default" ||
						!d.source.slice(rulePrefix.length).includes("/"))),
	);
	const collection = useCollection(rows, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				Recent deliveries and failures for this{" "}
				{scope.rule ? "rule" : "event bus"}. Accepted events can still fail
				delivery. History is bounded and resets on restart; payloads may be
				truncated.
			</Alert>
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				{...collection.collectionProps}
				items={collection.items}
				loading={poll.loading}
				wrapLines
				header={
					<Header
						variant="h2"
						counter={`(${rows.length})`}
						actions={
							<Button
								iconName="refresh"
								ariaLabel="Refresh deliveries"
								onClick={poll.reload}
							/>
						}
					>
						Deliveries
					</Header>
				}
				filter={
					<TextFilter
						{...collection.filterProps}
						filteringPlaceholder="Find deliveries"
					/>
				}
				pagination={<Pagination {...collection.paginationProps} />}
				columnDefinitions={[
					{
						id: "time",
						header: "Time",
						sortingField: "time",
						cell: (d) => (
							<Link onFollow={() => setSelected(d)}>
								{new Date(d.time).toLocaleString()}
							</Link>
						),
					},
					{ id: "rule", header: "Rule", cell: (d) => d.source },
					{ id: "target", header: "Target", cell: (d) => d.target },
					{ id: "status", header: "Status", cell: (d) => d.status },
					{ id: "error", header: "Error", cell: (d) => d.error || "—" },
				]}
				empty={
					<Box textAlign="center" padding="l">
						No matching deliveries. Send an event that matches an enabled rule.
					</Box>
				}
			/>
			{selected && (
				<Modal
					visible
					size="large"
					header="Delivery details"
					closeAriaLabel="Close delivery"
					onDismiss={() => setSelected(undefined)}
					footer={
						<Box float="right">
							<Button onClick={() => setSelected(undefined)}>Close</Button>
						</Box>
					}
				>
					<SpaceBetween size="l">
						<Box>
							{selected.status} · {selected.error || selected.target}
						</Box>
						<CodeBlock text={pretty(selected.payload)} />
					</SpaceBetween>
				</Modal>
			)}
		</SpaceBetween>
	);
}
