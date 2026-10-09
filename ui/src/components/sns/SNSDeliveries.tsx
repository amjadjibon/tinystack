import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Header,
	KeyValuePairs,
	Link,
	Modal,
	Pagination,
	SpaceBetween,
	StatusIndicator,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { api, type Delivery, pretty } from "#/lib/api";
import { usePoll } from "#/lib/usePoll";

export function SNSDeliveries({ arn }: { arn: string }) {
	const deliveries = usePoll(api.deliveries);
	const [selected, setSelected] = useState<Delivery | null>(null);
	const rows = (deliveries.data ?? []).filter(
		(d) => d.via === "sns" && d.source === arn,
	);
	const collection = useCollection(rows, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				Recent deliveries from this topic, including confirmation requests. HTTP
				delivery is asynchronous; a successful publish does not guarantee
				delivery. Filtered-out messages do not produce delivery records. History
				is bounded and resets on restart.
			</Alert>
			{deliveries.error && (
				<Alert type="error" header="Cannot load deliveries">
					{deliveries.error}
				</Alert>
			)}
			<Table
				{...collection.collectionProps}
				items={collection.items}
				loading={deliveries.loading}
				loadingText="Loading topic deliveries"
				trackBy={(d) => `${d.time}/${d.target}`}
				header={
					<Header
						variant="h2"
						counter={`(${rows.length})`}
						actions={
							<Button
								iconName="refresh"
								ariaLabel="Refresh topic deliveries"
								onClick={deliveries.reload}
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
					{ id: "target", header: "Target", cell: (d) => d.target },
					{
						id: "status",
						header: "Status",
						cell: (d) => (
							<StatusIndicator
								type={d.status === "delivered" ? "success" : "error"}
							>
								{d.status}
							</StatusIndicator>
						),
					},
					{ id: "error", header: "Error", cell: (d) => d.error || "—" },
				]}
				empty={
					<Box textAlign="center" padding="l">
						No matching topic deliveries
					</Box>
				}
			/>
			{selected && (
				<Modal
					visible
					size="large"
					header="Delivery details"
					closeAriaLabel="Close delivery details"
					onDismiss={() => setSelected(null)}
					footer={
						<Box float="right">
							<Button onClick={() => setSelected(null)}>Close</Button>
						</Box>
					}
				>
					<SpaceBetween size="l">
						<KeyValuePairs
							columns={2}
							items={[
								{ label: "Target", value: selected.target },
								{ label: "Status", value: selected.status },
								{
									label: "Time",
									value: new Date(selected.time).toLocaleString(),
								},
								{ label: "Error", value: selected.error || "—" },
							]}
						/>
						<Box variant="h3">Payload ({selected.size} bytes)</Box>
						<CodeBlock text={pretty(selected.payload)} />
					</SpaceBetween>
				</Modal>
			)}
		</SpaceBetween>
	);
}
