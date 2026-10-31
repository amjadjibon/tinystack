import {
	Alert,
	Button,
	Header,
	SpaceBetween,
	Table,
	type TableProps,
} from "@cloudscape-design/components";
import type { Poll } from "#/lib/usePoll";
export function DataTable<T>({
	title,
	poll,
	columns,
	actions,
}: {
	title: string;
	poll: Poll<T[]>;
	columns: TableProps.ColumnDefinition<T>[];
	actions?: React.ReactNode;
}) {
	return (
		<SpaceBetween size="s">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				items={poll.data ?? []}
				loading={poll.loading}
				loadingText={`Loading ${title}`}
				empty={`No ${title.toLowerCase()}.`}
				columnDefinitions={columns}
				header={
					<Header
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								{actions}
								<Button onClick={poll.reload}>Refresh</Button>
							</SpaceBetween>
						}
					>
						{title}
					</Header>
				}
			/>
		</SpaceBetween>
	);
}
