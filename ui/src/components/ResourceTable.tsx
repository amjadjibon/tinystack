import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Button,
	CollectionPreferences,
	type CollectionPreferencesProps,
	CopyToClipboard,
	Header,
	Link,
	Pagination,
	PropertyFilter,
	type PropertyFilterProps,
	Table,
	type TableProps,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { Resource } from "#/lib/api";
import { type IAMKind, iamHref, iamKinds } from "#/lib/iam";
import { s3Href } from "#/lib/s3";
import { snsHref } from "#/lib/sns";
import { sqsHref } from "#/lib/sqs";
import { useFollow } from "#/lib/useFollow";
import { EmptyState } from "./EmptyState";
import { ServiceLabel } from "./ServiceIcon";

export function resourceHref(r: Resource): string {
	if (r.service === "sns" && r.type === "topic")
		return snsHref({ account: r.account, region: r.region, topic: r.name });
	if (r.service === "sqs" && r.type === "queue")
		return sqsHref({ account: r.account, region: r.region, queue: r.name });
	if (r.service === "s3" && r.type === "bucket") {
		return s3Href({ account: r.account, region: r.region, bucket: r.name });
	}
	if (r.service === "iam" && iamKinds.includes(r.type as IAMKind)) {
		return iamHref(r.type as IAMKind, r.account, r.name);
	}
	const search = new URLSearchParams({
		type: r.type,
		name: r.name,
		account: r.account,
		region: r.region,
	});
	return `/services/${r.service}/resource?${search}`;
}

const filteringProperties: PropertyFilterProps.FilteringProperty[] = [
	{
		key: "name",
		propertyLabel: "Name",
		groupValuesLabel: "Names",
		operators: [":", "!:", "=", "!="],
	},
	{
		key: "type",
		propertyLabel: "Type",
		groupValuesLabel: "Types",
		operators: ["=", "!="],
	},
	{
		key: "service",
		propertyLabel: "Service",
		groupValuesLabel: "Services",
		operators: ["=", "!="],
	},
	{
		key: "region",
		propertyLabel: "Region",
		groupValuesLabel: "Regions",
		operators: ["=", "!="],
	},
	{
		key: "account",
		propertyLabel: "Account",
		groupValuesLabel: "Accounts",
		operators: ["=", "!="],
	},
	{
		key: "arn",
		propertyLabel: "ARN",
		groupValuesLabel: "ARNs",
		operators: [":", "!:", "="],
	},
];

const pageSizes = [10, 20, 50, 100].map((value) => ({
	value,
	label: `${value} resources`,
}));

// ResourceTable lists inventory resources the way console list pages do:
// a property filter, sortable columns, paging, and column preferences.
export function ResourceTable({
	title,
	resources,
	loading,
	onRefresh,
	showService,
}: {
	title: string;
	resources: Resource[];
	loading: boolean;
	onRefresh: () => void;
	showService?: boolean;
}) {
	const follow = useFollow();
	const columns: TableProps.ColumnDefinition<Resource>[] = [
		{
			id: "name",
			header: "Name",
			cell: (r) => (
				<Link href={resourceHref(r)} onFollow={follow}>
					{r.name}
				</Link>
			),
			sortingField: "name",
			isRowHeader: true,
		},
		{ id: "type", header: "Type", cell: (r) => r.type, sortingField: "type" },
		...(showService
			? [
					{
						id: "service",
						header: "Service",
						cell: (r: Resource) => <ServiceLabel name={r.service} />,
						sortingField: "service",
					},
				]
			: []),
		{
			id: "region",
			header: "Region",
			cell: (r) => r.region || "global",
			sortingField: "region",
		},
		{
			id: "account",
			header: "Account",
			cell: (r) => r.account,
			sortingField: "account",
		},
		{
			id: "arn",
			header: "ARN",
			width: 360,
			cell: (r) =>
				r.arn ? (
					<CopyToClipboard
						variant="inline"
						textToCopy={r.arn}
						copySuccessText="ARN copied"
						copyErrorText="ARN failed to copy"
					/>
				) : (
					"-"
				),
		},
		{
			id: "tags",
			header: "Tags",
			cell: (r) => Object.keys(r.tags ?? {}).length,
		},
	];

	const [preferences, setPreferences] =
		useState<CollectionPreferencesProps.Preferences>({
			pageSize: 20,
			contentDisplay: columns.map((c) => ({ id: c.id ?? "", visible: true })),
		});

	const {
		items,
		actions,
		collectionProps,
		propertyFilterProps,
		paginationProps,
		filteredItemsCount,
	} = useCollection(resources, {
		propertyFiltering: {
			filteringProperties,
			empty: (
				<EmptyState
					title="No resources"
					subtitle="Create one with the AWS CLI or an SDK."
				/>
			),
			noMatch: (
				<EmptyState
					title="No matches"
					subtitle="No resources match the filter."
					action={
						<Button
							onClick={() =>
								actions.setPropertyFiltering({ tokens: [], operation: "and" })
							}
						>
							Clear filter
						</Button>
					}
				/>
			),
		},
		sorting: { defaultState: { sortingColumn: columns[0] } },
		pagination: { pageSize: preferences.pageSize },
	});

	return (
		<Table
			{...collectionProps}
			variant="full-page"
			stickyHeader
			resizableColumns
			loading={loading}
			loadingText="Loading resources"
			items={items}
			trackBy={(r) =>
				`${r.service}/${r.account}/${r.region}/${r.type}/${r.name}`
			}
			columnDefinitions={columns}
			columnDisplay={preferences.contentDisplay}
			header={
				<Header
					variant="awsui-h1-sticky"
					counter={`(${resources.length})`}
					actions={
						<Button
							iconName="refresh"
							ariaLabel="Refresh"
							onClick={onRefresh}
						/>
					}
				>
					{title}
				</Header>
			}
			filter={
				<PropertyFilter
					{...propertyFilterProps}
					countText={`${filteredItemsCount ?? 0} matches`}
					filteringPlaceholder="Find resources"
					expandToViewport
				/>
			}
			pagination={<Pagination {...paginationProps} />}
			preferences={
				<CollectionPreferences
					title="Preferences"
					confirmLabel="Confirm"
					cancelLabel="Cancel"
					preferences={preferences}
					onConfirm={({ detail }) => setPreferences(detail)}
					pageSizePreference={{ title: "Page size", options: pageSizes }}
					contentDisplayPreference={{
						title: "Column preferences",
						options: columns.map((c) => ({
							id: c.id ?? "",
							label: String(c.header),
							alwaysVisible: c.id === "name",
						})),
					}}
				/>
			}
		/>
	);
}
