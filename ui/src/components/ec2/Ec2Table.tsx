import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Box,
	Header,
	Pagination,
	Table,
	type TableProps,
	TextFilter,
} from "@cloudscape-design/components";
import type { ReactNode } from "react";
import type { Ec2Row } from "#/lib/ec2";
export function Ec2Table({
	title,
	description,
	items: source,
	columns,
	loading,
	actions,
	trackBy,
	page = false,
}: {
	title: string;
	description?: string;
	items: Ec2Row[];
	columns: TableProps.ColumnDefinition<Ec2Row>[];
	loading?: boolean;
	actions?: ReactNode;
	trackBy: string | ((row: Ec2Row) => string);
	page?: boolean;
}) {
	const {
		items,
		collectionProps,
		filterProps,
		paginationProps,
		filteredItemsCount,
	} = useCollection(source, {
		filtering: {
			filteringFunction: (row, query) =>
				JSON.stringify(row).toLowerCase().includes(query.toLowerCase()),
			empty: (
				<Box padding="l" textAlign="center">
					No {title.toLowerCase()} to display.
				</Box>
			),
			noMatch: (
				<Box padding="l" textAlign="center">
					No matches. Clear or change your search.
				</Box>
			),
		},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	return (
		<Table
			{...collectionProps}
			variant={page ? "full-page" : "container"}
			stickyHeader={page}
			items={items}
			trackBy={trackBy}
			loading={loading}
			loadingText={`Loading ${title.toLowerCase()}`}
			columnDefinitions={columns}
			header={
				<Header
					variant={page ? "h1" : "h2"}
					counter={`(${source.length})`}
					description={description}
					actions={actions}
				>
					{title}
				</Header>
			}
			filter={
				<TextFilter
					{...filterProps}
					filteringPlaceholder={`Find ${title.toLowerCase()}`}
					countText={`${filteredItemsCount ?? 0} matches`}
				/>
			}
			pagination={<Pagination {...paginationProps} />}
		/>
	);
}
