import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Box,
	Button,
	Header,
	Hotspot,
	Link,
	Pagination,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { Resource } from "#/lib/api";
import { type EventScope, eventHref, inventoryRule } from "#/lib/eventbridge";
import { useFollow } from "#/lib/useFollow";
import { HOTSPOT } from "#/tutorials/hotspots";
import { EventRuleForm } from "./EventRuleForm";
export function EventRuleList({
	scope,
	resources,
	enabled,
	loading,
	onChanged,
	onOpen,
}: {
	scope: EventScope;
	resources: Resource[];
	enabled: boolean;
	loading: boolean;
	onChanged: (message: string) => void;
	onOpen: (name: string) => void;
}) {
	const follow = useFollow(),
		[creating, setCreating] = useState(false);
	const rows = resources
		.filter(
			(r) =>
				r.service === "eventbridge" &&
				r.type === "rule" &&
				r.account === scope.account &&
				r.region === scope.region &&
				inventoryRule(r).EventBusName === (scope.bus || "default"),
		)
		.map((r) => ({ ...inventoryRule(r), name: r.name, arn: r.arn }));
	const collection = useCollection(rows, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	return (
		<>
			<Table
				{...collection.collectionProps}
				items={collection.items}
				loading={loading}
				trackBy="arn"
				wrapLines
				header={
					<Header
						variant="h2"
						counter={`(${rows.length})`}
						description={`Rules on ${scope.bus || "default"}.`}
						actions={
							<Hotspot
								hotspotId={HOTSPOT.eventsCreateRule}
								side="left"
								direction="left"
							>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => setCreating(true)}
								>
									Create rule
								</Button>
							</Hotspot>
						}
					>
						Rules
					</Header>
				}
				filter={
					<TextFilter
						{...collection.filterProps}
						filteringPlaceholder="Find rules"
					/>
				}
				pagination={<Pagination {...collection.paginationProps} />}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						sortingField: "name",
						cell: (r) => (
							<Link
								href={eventHref(
									{ ...scope, bus: scope.bus || "default", rule: r.name },
									"rules",
								)}
								onFollow={follow}
							>
								{r.name}
							</Link>
						),
					},
					{ id: "state", header: "State", cell: (r) => r.State },
					{
						id: "pattern",
						header: "Event pattern",
						cell: (r) => (r.EventPattern ? "Configured" : "—"),
					},
					{
						id: "schedule",
						header: "Schedule",
						cell: (r) => r.ScheduleExpression || "—",
					},
					{
						id: "description",
						header: "Description",
						cell: (r) => r.Description || "—",
					},
				]}
				empty={
					<Box textAlign="center" padding="l">
						<SpaceBetween size="s">
							<Box>No matching rules.</Box>
							<Box>
								Create a rule to route events or run targets on a schedule.
							</Box>
						</SpaceBetween>
					</Box>
				}
			/>
			{creating && (
				<EventRuleForm
					scope={{ ...scope, bus: scope.bus || "default" }}
					onClose={() => setCreating(false)}
					onSaved={(name) => {
						setCreating(false);
						onChanged(`Rule ${name} created.`);
						onOpen(name);
					}}
				/>
			)}
		</>
	);
}
