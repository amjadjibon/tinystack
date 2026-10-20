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
	detail?: (
		item: T,
		context: { records: T[]; select: (item: T) => void },
	) => ReactNode;
	filters?: { label: string; matches: (item: T) => boolean }[];
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
	filters = [],
	empty,
}: LogTableProps<T>) {
	const [live, setLive] = useState(true);
	const [activeFilters, setActiveFilters] = useState<string[]>([]);
	const { data, loading, reload } = usePoll(fetcher, live ? 3000 : 0);
	const [selected, setSelected] = useState<T>();
	const [panelOpen, setPanelOpen] = useState(false);

	const {
		items,
		collectionProps,
		filterProps,
		paginationProps,
		filteredItemsCount,
	} = useCollection(
		(data ?? []).filter((item) =>
			filters.every(
				(filter) =>
					!activeFilters.includes(filter.label) || filter.matches(item),
			),
		),
		{
			filtering: {
				filteringFunction: filterText
					? (item, text) =>
							filterText(item).toLowerCase().includes(text.toLowerCase())
					: undefined,
				empty: (
					<EmptyState
						title={data?.length ? "No matches" : "Nothing here yet"}
						subtitle={data?.length ? "No entries match the filters." : empty}
					/>
				),
				noMatch: (
					<EmptyState
						title="No matches"
						subtitle="No entries match the filter."
					/>
				),
			},
			pagination: { pageSize: 50 },
			sorting: {},
		},
	);

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
							detail(selected, {
								records: data ?? [],
								select: (item) => {
									setSelected(item);
									setPanelOpen(true);
								},
							})
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
						<SpaceBetween size="s">
							{filters.length > 0 && (
								<SpaceBetween direction="horizontal" size="m">
									{filters.map((filter) => (
										<Toggle
											key={filter.label}
											checked={activeFilters.includes(filter.label)}
											onChange={({ detail: d }) =>
												setActiveFilters((current) =>
													d.checked
														? [...current, filter.label]
														: current.filter((label) => label !== filter.label),
												)
											}
										>
											{filter.label}
										</Toggle>
									))}
								</SpaceBetween>
							)}
							<TextFilter
								{...filterProps}
								filteringPlaceholder="Find entries"
								countText={`${filteredItemsCount ?? 0} matches`}
							/>
						</SpaceBetween>
					}
					pagination={<Pagination {...paginationProps} />}
				/>
			}
		/>
	);
}
