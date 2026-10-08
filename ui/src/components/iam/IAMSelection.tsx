import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Box,
	Header,
	Pagination,
	Table,
	TextFilter,
} from "@cloudscape-design/components";

export type IAMOption = { label: string; value: string; description?: string };

export function IAMSelection({
	options,
	selected,
	onChange,
	title,
	single = false,
	disabled = false,
}: {
	options: IAMOption[];
	selected: string[];
	onChange: (selected: string[]) => void;
	title: string;
	single?: boolean;
	disabled?: boolean;
}) {
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(options, {
			filtering: {
				empty: <Box>No resources available.</Box>,
				noMatch: <Box>No matching resources.</Box>,
			},
			pagination: { pageSize: 10 },
			sorting: {},
		});
	return (
		<Table
			{...collectionProps}
			items={items}
			trackBy="value"
			selectionType={single ? "single" : "multi"}
			isItemDisabled={() => disabled}
			selectedItems={options.filter((option) =>
				selected.includes(option.value),
			)}
			onSelectionChange={({ detail }) =>
				onChange(detail.selectedItems.map((option) => option.value))
			}
			ariaLabels={{
				selectionGroupLabel: title,
				allItemsSelectionLabel: () => "Select all",
				itemSelectionLabel: (_, item) => `Select ${item.label}`,
			}}
			header={
				<Header variant="h2" counter={`(${selected.length} selected)`}>
					{title}
				</Header>
			}
			filter={
				<TextFilter
					{...filterProps}
					filteringPlaceholder={`Find ${title.toLowerCase()}`}
					filteringAriaLabel={`Find ${title.toLowerCase()}`}
				/>
			}
			pagination={
				<Pagination
					{...paginationProps}
					ariaLabels={{
						nextPageLabel: "Next page",
						previousPageLabel: "Previous page",
						pageLabel: (page) => `Page ${page}`,
					}}
				/>
			}
			columnDefinitions={[
				{
					id: "name",
					header: "Name",
					cell: (option) => option.label,
					sortingField: "label",
				},
				{
					id: "description",
					header: "Details",
					cell: (option) => option.description ?? option.value,
				},
			]}
		/>
	);
}
