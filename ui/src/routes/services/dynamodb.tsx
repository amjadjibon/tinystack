import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Container,
	Flashbar,
	FormField,
	Header,
	Hotspot,
	Link,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import {
	createFileRoute,
	useLocation,
	useNavigate,
} from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { DynamoTable } from "#/components/dynamodb/DynamoTable";
import { DynamoTableForm } from "#/components/dynamodb/DynamoTableForm";
import type { Resource } from "#/lib/api";
import { type DynamoScope, dynamoHref, tableDetails } from "#/lib/dynamodb";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { HOTSPOT } from "#/tutorials/hotspots";

export const Route = createFileRoute("/services/dynamodb")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		table: String(search.table ?? ""),
		section: search.section === "items" ? "items" : "tables",
		create: search.create === true || search.create === "true",
	}),
	component: DynamoPage,
});
function DynamoPage() {
	const search = Route.useSearch(),
		navigate = useNavigate(),
		location = useLocation();
	const { canWrite, account, region, inventory, capabilities } = useScope();
	const scope = { account, region, table: search.table };
	const [notice, setNotice] = useState("");
	const context = `${account}/${region}/${search.table}/${search.section}/${search.create}/${location.pathname}`;
	const current = useRef(context),
		mounted = useRef(true);
	current.current = context;
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	// biome-ignore lint/correctness/useExhaustiveDependencies: notices belong to the selected account and Region.
	useEffect(() => setNotice(""), [account, region]);
	const actionable =
		canWrite && /^\d{12}$/.test(account) && validRegion(region);
	const enabled = !!capabilities.data?.pages.dynamodb && actionable;
	const tables = (inventory.data ?? []).filter(
		(r) =>
			r.service === "dynamodb" &&
			r.type === "table" &&
			r.account === account &&
			r.region === region,
	);
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current && current.current === context) setNotice(message);
	};
	const go = (table = "", create = false, section = "tables") => {
		if (!mounted.current || current.current !== context) return;
		return navigate({
			to: "/services/dynamodb",
			search: { account, region, table, create, section },
		});
	};
	const listHref = dynamoHref({ account, region }),
		itemsHref = dynamoHref({ account, region }, false, "items");
	return (
		<ConsoleLayout
			contentType={
				search.table || search.create || search.section === "items"
					? "default"
					: "table"
			}
			navigationHeader={{ text: "Amazon DynamoDB", href: listHref }}
			activeHref={search.section === "items" ? itemsHref : listHref}
			navigationItems={[
				{ type: "link", text: "Tables", href: listHref },
				{ type: "link", text: "Explore items", href: itemsHref },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Amazon DynamoDB", href: listHref },
				{
					text: search.section === "items" ? "Explore items" : "Tables",
					href: search.section === "items" ? itemsHref : listHref,
				},
				...(search.create
					? [
							{
								text: "Create table",
								href: dynamoHref({ account, region }, true),
							},
						]
					: search.table
						? [{ text: search.table, href: dynamoHref(scope) }]
						: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!notice && (
						<Flashbar
							items={[
								{
									id: "dynamodb-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error" header="Cannot load DynamoDB">
							{inventory.error || capabilities.error}
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info" header="DynamoDB management is read-only">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.dynamodb && (
						<Alert type="warning">
							DynamoDB is not enabled on this server.
						</Alert>
					)}
					{search.section === "items" && !search.create && (
						<Container header={<Header variant="h2">Explore items</Header>}>
							<FormField label="Table">
								<Select
									selectedOption={
										search.table
											? { value: search.table, label: search.table }
											: null
									}
									placeholder="Choose a table"
									options={tables.map((t) => ({
										value: t.name,
										label: t.name,
									}))}
									onChange={({ detail }) =>
										go(detail.selectedOption.value ?? "", false, "items")
									}
								/>
							</FormField>
						</Container>
					)}
					{search.create ? (
						enabled ? (
							<DynamoTableForm
								key={`${account}/${region}`}
								scope={scope}
								onCancel={() => go()}
								onSaved={(name) => {
									changed(`Table ${name} created.`);
									go(name);
								}}
							/>
						) : (
							<Box>
								Table creation is unavailable in the current server mode.
							</Box>
						)
					) : search.table ? (
						<DynamoTable
							key={`${account}/${region}/${search.table}/${enabled}`}
							scope={scope}
							resource={tables.find((t) => t.name === search.table)}
							resources={inventory.data ?? []}
							enabled={enabled}
							lambdaEnabled={!!capabilities.data?.pages.lambda && actionable}
							loading={inventory.loading}
							initialTab="items"
							onRefresh={inventory.reload}
							onChanged={changed}
							onDeleted={() => {
								changed(`Table ${search.table} deleted.`);
								go();
							}}
						/>
					) : search.section === "items" ? (
						<Box>Choose a table to query, scan or edit its items.</Box>
					) : (
						<TableList
							key={`${account}/${region}`}
							scope={scope}
							tables={tables}
							enabled={enabled}
							loading={inventory.loading}
							onRefresh={inventory.reload}
							onCreate={() => go("", true)}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}
function TableList({
	scope,
	tables,
	enabled,
	loading,
	onRefresh,
	onCreate,
}: {
	scope: DynamoScope;
	tables: Resource[];
	enabled: boolean;
	loading: boolean;
	onRefresh: () => void;
	onCreate: () => void;
}) {
	const follow = useFollow();
	const rows = tables.map((r) => ({ ...r, ...tableDetails(r) }));
	const {
		items,
		collectionProps,
		filterProps,
		paginationProps,
		filteredItemsCount,
	} = useCollection(rows, {
		filtering: {
			empty: (
				<Box textAlign="center" padding="l">
					No tables. Create a table to store and explore items.
				</Box>
			),
			noMatch: (
				<Box textAlign="center" padding="l">
					No tables match your search.
				</Box>
			),
		},
		sorting: { defaultState: { sortingColumn: { sortingField: "name" } } },
		pagination: { pageSize: 20 },
	});
	return (
		<Table
			{...collectionProps}
			variant="full-page"
			stickyHeader
			resizableColumns
			items={items}
			trackBy="arn"
			loading={loading}
			loadingText="Loading tables"
			header={
				<Header
					variant="awsui-h1-sticky"
					counter={`(${rows.length})`}
					description="Tables in the selected account and Region."
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh tables"
								onClick={onRefresh}
							/>
							<Hotspot
								hotspotId={HOTSPOT.dynamoCreate}
								side="left"
								direction="left"
							>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={onCreate}
								>
									Create table
								</Button>
							</Hotspot>
						</SpaceBetween>
					}
				>
					Tables
				</Header>
			}
			filter={
				<TextFilter
					{...filterProps}
					filteringPlaceholder="Find tables"
					countText={`${filteredItemsCount ?? 0} matches`}
				/>
			}
			pagination={<Pagination {...paginationProps} />}
			columnDefinitions={[
				{
					id: "name",
					header: "Table name",
					isRowHeader: true,
					sortingField: "name",
					cell: (t) => (
						<Link
							href={dynamoHref({ ...scope, table: t.name })}
							onFollow={follow}
						>
							{t.name}
						</Link>
					),
				},
				{
					id: "state",
					header: "Status",
					sortingField: "TableStatus",
					cell: (t) => t.TableStatus,
				},
				{
					id: "partition",
					header: "Partition key",
					cell: (t) =>
						t.KeySchema?.find((k) => k.KeyType === "HASH")?.AttributeName,
				},
				{
					id: "sort",
					header: "Sort key",
					cell: (t) =>
						t.KeySchema?.find((k) => k.KeyType === "RANGE")?.AttributeName ??
						"—",
				},
				{
					id: "mode",
					header: "Capacity mode",
					cell: (t) => t.BillingModeSummary?.BillingMode ?? "PROVISIONED",
				},
				{
					id: "items",
					header: "Item count",
					sortingField: "ItemCount",
					cell: (t) => t.ItemCount ?? 0,
				},
				{
					id: "size",
					header: "Table size",
					sortingField: "TableSizeBytes",
					cell: (t) => `${t.TableSizeBytes ?? 0} bytes`,
				},
			]}
		/>
	);
}
