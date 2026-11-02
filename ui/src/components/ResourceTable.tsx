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
	SpaceBetween,
	Table,
	type TableProps,
} from "@cloudscape-design/components";
import { type ReactNode, useState } from "react";
import { acmHref } from "#/lib/acm";
import type { Resource } from "#/lib/api";
import { gatewayHref } from "#/lib/apigateway";
import { cfnHref } from "#/lib/cloudformation";
import { cloudwatchHref } from "#/lib/cloudwatch";
import { cognitoHref } from "#/lib/cognito";
import { costHref } from "#/lib/cost";
import { dynamoHref } from "#/lib/dynamodb";
import { ec2Href, ec2Kinds, ec2Sections } from "#/lib/ec2";
import { elasticacheHref } from "#/lib/elasticache";
import { elbv2Href } from "#/lib/elbv2";
import { eventHref, inventoryRule } from "#/lib/eventbridge";
import { type IAMKind, iamHref, iamKinds } from "#/lib/iam";
import { kmsHref } from "#/lib/kms";
import { lambdaHref } from "#/lib/lambda";
import { logsHref } from "#/lib/logs";
import { memorydbHref } from "#/lib/memorydb";
import { pipesHref } from "#/lib/pipes";
import { rdsHref } from "#/lib/rds";
import { route53Href } from "#/lib/route53";
import { s3Href } from "#/lib/s3";
import { schedulerHref } from "#/lib/scheduler";
import { secretHref } from "#/lib/secretsmanager";
import { sesv2Href } from "#/lib/sesv2";
import { sfnHref } from "#/lib/sfn";
import { snsHref } from "#/lib/sns";
import { sqsHref } from "#/lib/sqs";
import { ssmHref } from "#/lib/ssm";
import { streamHref } from "#/lib/streamingHelpers";
import { useFollow } from "#/lib/useFollow";
import { EmptyState } from "./EmptyState";
import { ServiceLabel } from "./ServiceIcon";

export function resourceHref(r: Resource): string {
	if (r.service === "ecr" || r.service === "ecs") {
		return `/services/${r.service}?${new URLSearchParams({ account: r.account, region: r.region })}`;
	}

	if (r.service === "elbv2")
		return elbv2Href({ account: r.account, region: r.region });

	if (r.service === "memorydb")
		return memorydbHref({ account: r.account, region: r.region });

	if (r.service === "sesv2")
		return sesv2Href({ account: r.account, region: r.region });
	if (r.service === "pipes")
		return pipesHref({ account: r.account, region: r.region });
	if (r.service === "scheduler")
		return schedulerHref({ account: r.account, region: r.region });

	if (
		r.service === "budgets" ||
		r.service === "costexplorer" ||
		r.service === "costandusagereportservice"
	)
		return costHref(
			r.service,
			r.account,
			r.type,
			r.service === "costexplorer" ? r.arn : r.name,
		);
	if (r.service === "cognitoidp" && r.type === "user-pool")
		return cognitoHref({
			account: r.account,
			region: r.region,
			id: r.arn.split("/").at(-1),
		});
	if (r.service === "kinesis" || r.service === "firehose")
		return streamHref(r.service, {
			account: r.account,
			region: r.region,
			stream: r.name,
		});
	if (r.service === "rds")
		return rdsHref({
			account: r.account,
			region: r.region,
			...(r.type === "db-instance"
				? { database: r.name }
				: { section: r.type === "db-subnet-group" ? "subnets" : "parameters" }),
		});
	if (r.service === "elasticache") {
		const section = {
			"cache-cluster": "clusters",
			"cache-replication-group": "groups",
			"serverless-cache": "serverless",
			"cache-subnet-group": "subnets",
			"cache-parameter-group": "parameters",
		}[r.type];
		if (section)
			return elasticacheHref({ account: r.account, region: r.region, section });
	}
	if (r.service === "cloudformation" && r.type === "stack")
		return cfnHref({ account: r.account, region: r.region, stack: r.arn });
	if (r.service === "kms" && r.type === "key")
		return kmsHref({
			account: r.account,
			region: r.region,
			key: r.arn || r.name,
		});
	if (r.service === "acm" && r.type === "certificate")
		return acmHref({
			account: r.account,
			region: r.region,
			certificate: r.arn,
		});
	if (r.service === "ssm" && r.type === "parameter")
		return ssmHref({ account: r.account, region: r.region, parameter: r.name });
	if (r.service === "route53" && r.type === "hosted-zone")
		return route53Href({
			account: r.account,
			zone: String(r.details?.Id ?? r.arn.split("/").at(-1)),
		});
	if (r.service === "route53" && r.type === "health-check")
		return `${route53Href({ account: r.account })}&section=healthchecks`;
	if (r.service === "secretsmanager" && r.type === "secret")
		return secretHref({
			account: r.account,
			region: r.region,
			secret: r.arn || r.name,
		});
	if (r.service === "ec2") {
		const section = ec2Sections.find((k) => ec2Kinds[k].type === r.type);
		if (section)
			return ec2Href({
				account: r.account,
				region: r.region,
				area: ["instances", "keys", "images"].includes(section) ? "ec2" : "vpc",
				section,
				id: r.arn.split("/").at(-1) ?? r.name,
			});
	}
	if (r.service === "sfn" && r.type === "state-machine")
		return sfnHref({ account: r.account, region: r.region, machine: r.name });
	if (
		(r.service === "apigateway" && r.type === "rest-api") ||
		(r.service === "apigatewayv2" && r.type === "http-api")
	)
		return gatewayHref(
			{
				account: r.account,
				region: r.region,
				kind: r.service as "apigateway" | "apigatewayv2",
				apiId: String(r.details?.ApiId ?? ""),
			},
			"routes",
		);
	if (
		r.service === "cloudwatch" &&
		(r.type === "alarm" || r.type === "dashboard")
	)
		return cloudwatchHref(
			{ account: r.account, region: r.region, [r.type]: r.name },
			r.type === "alarm" ? "alarms" : "dashboards",
		);
	if (r.service === "logs" && r.type === "log-group")
		return logsHref({ account: r.account, region: r.region, group: r.name });
	if (
		r.service === "eventbridge" &&
		(r.type === "event-bus" || r.type === "rule")
	)
		return eventHref(
			{
				account: r.account,
				region: r.region,
				bus:
					r.type === "rule"
						? (inventoryRule(r).EventBusName ?? "default")
						: r.name,
				rule: r.type === "rule" ? r.name : undefined,
			},
			r.type === "rule" ? "rules" : "buses",
		);
	if (r.service === "dynamodb" && r.type === "table")
		return dynamoHref({ account: r.account, region: r.region, table: r.name });
	if (r.service === "lambda" && r.type === "function")
		return lambdaHref({
			account: r.account,
			region: r.region,
			functionName: r.name,
		});
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
	renderActions,
	extraColumns = [],
	emptyText = "Create one with the AWS CLI or an SDK.",
}: {
	title: string;
	resources: Resource[];
	loading: boolean;
	onRefresh: () => void;
	showService?: boolean;
	renderActions?: (resource: Resource) => ReactNode;
	extraColumns?: TableProps.ColumnDefinition<Resource>[];
	emptyText?: string;
}) {
	const follow = useFollow();
	const [selected, setSelected] = useState<Resource[]>([]);
	const current = resources.find(
		(r) =>
			r.arn === selected[0]?.arn &&
			r.name === selected[0]?.name &&
			r.type === selected[0]?.type,
	);
	const columns: TableProps.ColumnDefinition<Resource>[] = [
		...extraColumns,
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
			empty: <EmptyState title="No resources" subtitle={emptyText} />,
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
			selectionType={renderActions ? "single" : undefined}
			selectedItems={current ? [current] : []}
			onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
			ariaLabels={{
				selectionGroupLabel: "Select a resource",
				itemSelectionLabel: (_data, item) => `Select ${item.name}`,
			}}
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
						<SpaceBetween direction="horizontal" size="xs">
							{current && renderActions?.(current)}
							<Button
								iconName="refresh"
								ariaLabel="Refresh"
								onClick={onRefresh}
							/>
						</SpaceBetween>
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
