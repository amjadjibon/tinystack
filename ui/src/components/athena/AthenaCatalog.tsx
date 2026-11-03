import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useEffect, useState } from "react";
import { Action, Choice, Rows } from "#/components/containers/Controls";
import { type AthenaScope, athenaCall, athenaPages } from "#/lib/athena";
import { unixTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";

type Metadata = {
	Name: string;
	TableType?: string;
	CreateTime?: number;
	LastAccessTime?: number;
	Columns?: { Name?: string; Type?: string; Comment?: string }[];
	PartitionKeys?: { Name?: string; Type?: string }[];
	Parameters?: Record<string, string>;
};
export function AthenaCatalog({
	scope,
	enabled,
	group,
}: {
	scope: AthenaScope;
	enabled: boolean;
	group: string;
}) {
	const [catalog, setCatalog] = useState("AwsDataCatalog"),
		[database, setDatabase] = useState(""),
		[table, setTable] = useState("");
	const catalogs = usePoll(
		() =>
			enabled
				? athenaPages<{ CatalogName: string; Type?: string }>(
						scope,
						"ListDataCatalogs",
						"DataCatalogsSummary",
						{ WorkGroup: group },
					)
				: Promise.resolve([]),
		0,
	);
	const info = usePoll(
		() =>
			enabled
				? athenaCall(scope, "GetDataCatalog", {
						Name: catalog,
						WorkGroup: group,
					})
				: Promise.resolve({}),
		0,
	);
	const dbs = usePoll(
		() =>
			enabled
				? athenaPages<{ Name: string; Description?: string }>(
						scope,
						"ListDatabases",
						"DatabaseList",
						{ CatalogName: catalog, WorkGroup: group },
					)
				: Promise.resolve([]),
		0,
	);
	const tables = usePoll(
		() =>
			enabled && database
				? athenaPages<Metadata>(
						scope,
						"ListTableMetadata",
						"TableMetadataList",
						{ CatalogName: catalog, DatabaseName: database, WorkGroup: group },
					)
				: Promise.resolve([]),
		0,
	);
	const metadata = usePoll(
		async () =>
			enabled && database && table
				? (
						await athenaCall<{ TableMetadata: Metadata }>(
							scope,
							"GetTableMetadata",
							{
								CatalogName: catalog,
								DatabaseName: database,
								TableName: table,
								WorkGroup: group,
							},
						)
					).TableMetadata
				: undefined,
		0,
	);
	useEffect(() => {
		if (catalog) {
			info.reload();
			dbs.reload();
		}
	}, [catalog, info.reload, dbs.reload]);
	useEffect(() => {
		if (database) tables.reload();
	}, [database, tables.reload]);
	useEffect(() => {
		if (database && table) metadata.reload();
	}, [table, database, metadata.reload]);
	return (
		<SpaceBetween size="m">
			{[catalogs.error, info.error, dbs.error, tables.error, metadata.error]
				.filter(Boolean)
				.map((e) => (
					<Alert key={e} type="error">
						{e}
					</Alert>
				))}
			<Choice
				label="Catalog"
				value={catalog}
				options={(catalogs.data ?? []).map((c) => c.CatalogName)}
				onChange={(v) => {
					setCatalog(v);
					setDatabase("");
					setTable("");
				}}
			/>
			<pre>{JSON.stringify(info.data, null, 2)}</pre>
			<Choice
				label="Database"
				value={database}
				options={(dbs.data ?? []).map((d) => d.Name)}
				onChange={(v) => {
					setDatabase(v);
					setTable("");
				}}
			/>
			<Action
				disabled={!enabled}
				onClick={() => {
					catalogs.reload();
					dbs.reload();
					tables.reload();
					metadata.reload();
				}}
			>
				Refresh catalog
			</Action>
			<Rows
				title="Tables"
				items={tables.data ?? []}
				loading={tables.loading}
				emptyText="Choose a Glue database containing tables."
				columns={[
					{ id: "name", header: "Name", cell: (t) => t.Name },
					{ id: "type", header: "Type", cell: (t) => t.TableType },
					{
						id: "created",
						header: "Created",
						cell: (t) => unixTime(t.CreateTime),
					},
					{
						id: "access",
						header: "Last accessed",
						cell: (t) => unixTime(t.LastAccessTime),
					},
					{
						id: "action",
						header: "Actions",
						cell: (t) => (
							<Action onClick={() => setTable(t.Name)}>
								View columns and parameters
							</Action>
						),
					},
				]}
			/>
			{metadata.data && (
				<SpaceBetween size="m">
					<Rows
						title="Columns"
						items={metadata.data.Columns ?? []}
						emptyText="This table has no columns."
						columns={[
							{ id: "name", header: "Name", cell: (c) => c.Name },
							{ id: "type", header: "Type", cell: (c) => c.Type },
							{ id: "comment", header: "Comment", cell: (c) => c.Comment },
						]}
					/>
					<pre>
						{JSON.stringify(
							{
								PartitionKeys: metadata.data.PartitionKeys,
								Parameters: metadata.data.Parameters,
							},
							null,
							2,
						)}
					</pre>
				</SpaceBetween>
			)}
		</SpaceBetween>
	);
}
