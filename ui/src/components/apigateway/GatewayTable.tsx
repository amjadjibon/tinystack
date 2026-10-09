import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Box,
	Pagination,
	Table,
	type TableProps,
	TextFilter,
} from "@cloudscape-design/components";
import type { ReactNode } from "react";
export function GatewayTable<T>({
	items,
	header,
	...props
}: Omit<TableProps<T>, "items" | "header"> & {
	items: readonly T[];
	header?: ReactNode;
}) {
	const c = useCollection(items, {
		filtering: {},
		sorting: {},
		pagination: { pageSize: 20 },
	});
	return (
		<Table
			{...props}
			{...c.collectionProps}
			items={c.items}
			header={header}
			wrapLines
			filter={
				<TextFilter {...c.filterProps} filteringPlaceholder="Find resources" />
			}
			pagination={<Pagination {...c.paginationProps} />}
			empty={
				props.empty ?? (
					<Box textAlign="center" padding="l">
						No matching resources.
					</Box>
				)
			}
		/>
	);
}
