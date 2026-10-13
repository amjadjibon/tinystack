import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Box,
	Button,
	Header,
	Pagination,
	SpaceBetween,
	SplitPanel,
	Table,
	type TableProps,
	TextFilter,
	Toggle,
} from "@cloudscape-design/components";
import { type ReactNode, useState } from "react";
import { usePoll } from "#/lib/usePoll";
import { ConsoleLayout } from "./Console";
import { EmptyState } from "./EmptyState";

export type LogTableProps<T> = {
	title: string;
	description: string;
	fetcher: () => Promise<T[]>;
	columns: TableProps.ColumnDefinition<T>[];
	trackBy: (item: T) => string;
	filterText?: (item: T) => string;
	onClear?: () => Promise<void>;
	detailTitle?: (item: T) => string;
	detail?: (item: T) => ReactNode;
	empty: string;
};

// LogTable is a console page for one of TinyStack's live logs: it refreshes
// every few seconds while live, filters by text, and shows the selected
// entry in the split panel.
export function LogTable<T>({
	title,
	description,
	fetcher,
	columns,
	trackBy,
	filterText,
	onClear,
	detailTitle,
	detail,
	empty,
}: LogTableProps<T>) {
	const [live, setLive] = useState(true);
	const { data, loading, reload } = usePoll(fetcher, live ? 3000 : 0);
	const [selected, setSelected] = useState<T>();
	const [panelOpen, setPanelOpen] = useState(false);

	const {
		items,
		collectionProps,
		filterProps,
		paginationProps,
		filteredItemsCount,
	} = useCollection(data ?? [], {
		filtering: {
			filteringFunction: filterText
				? (item, text) =>
						filterText(item).toLowerCase().includes(text.toLowerCase())
				: undefined,
			empty: <EmptyState title="Nothing here yet" subtitle={empty} />,
			noMatch: (
				<EmptyState
					title="No matches"
					subtitle="No entries match the filter."
				/>
			),
		},
		pagination: { pageSize: 50 },
		sorting: {},
	});

	const clear = async () => {
		await onClear?.();
		setSelected(undefined);
		reload();
	};

	return (
		<ConsoleLayout
			breadcrumbs={[{ text: title, href: "#" }]}
			splitPanelOpen={!!detail && panelOpen}
			onSplitPanelToggle={setPanelOpen}
			splitPanel={
				detail && (
					<SplitPanel
						header={selected && detailTitle ? detailTitle(selected) : "Details"}
						hidePreferencesButton
					>
						{selected ? (
							detail(selected)
						) : (
							<Box color="text-status-inactive">
								Select an entry to see its details.
							</Box>
						)}
					</SplitPanel>
				)
			}
			content={
				<Table
					{...collectionProps}
					variant="full-page"
					stickyHeader
					resizableColumns
					wrapLines={false}
					loading={loading}
					loadingText="Loading"
					items={items}
					trackBy={trackBy}
					columnDefinitions={columns}
					selectionType={detail ? "single" : undefined}
					selectedItems={selected ? [selected] : []}
					onSelectionChange={({ detail: d }) => {
						setSelected(d.selectedItems[0]);
						setPanelOpen(true);
					}}
					onRowClick={
						detail
							? ({ detail: d }) => {
									setSelected(d.item);
									setPanelOpen(true);
								}
							: undefined
					}
					header={
						<Header
							variant="awsui-h1-sticky"
							counter={`(${data?.length ?? 0})`}
							description={description}
							actions={
								<SpaceBetween
									direction="horizontal"
									size="xs"
									alignItems="center"
								>
									<Toggle
										checked={live}
										onChange={({ detail: d }) => setLive(d.checked)}
									>
										Live
									</Toggle>
									<Button
										iconName="refresh"
										ariaLabel="Refresh"
										onClick={reload}
									/>
									{onClear && <Button onClick={clear}>Clear</Button>}
								</SpaceBetween>
							}
						>
							{title}
						</Header>
					}
					filter={
						<TextFilter
							{...filterProps}
							filteringPlaceholder="Find entries"
							countText={`${filteredItemsCount ?? 0} matches`}
						/>
					}
					pagination={<Pagination {...paginationProps} />}
				/>
			}
		/>
	);
}
