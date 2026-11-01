import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Container,
	CopyToClipboard,
	FormField,
	Header,
	Multiselect,
	Pagination,
	Select,
	SpaceBetween,
	StatusIndicator,
	Table,
	type TableProps,
	TextFilter,
} from "@cloudscape-design/components";
import type { ReactNode } from "react";
import { ConsoleTags } from "#/components/ConsoleTags";
import { api } from "#/lib/api";
import {
	type ElastiCacheScope,
	type EngineVersion,
	ecCall,
	ecPages,
	ecTags,
	type ParameterGroup,
	type SubnetGroup,
	statusType,
	type Tag,
} from "#/lib/elasticache";
import { usePoll } from "#/lib/usePoll";

export function CacheStatus({ status }: { status?: string }) {
	return (
		<StatusIndicator type={statusType(status)}>
			{status || "unknown"}
		</StatusIndicator>
	);
}

export const copyText = (text: string) => (
	<CopyToClipboard
		copyButtonAriaLabel="Copy"
		copyErrorText="Copy failed"
		copySuccessText="Copied"
		textToCopy={text}
	/>
);

// EndpointValue shows an endpoint with its copy button, or Pending before one exists.
export function EndpointValue({ value }: { value: string }) {
	return value ? (
		<SpaceBetween direction="horizontal" size="xs">
			<Box variant="code">{value}</Box>
			{copyText(value)}
		</SpaceBetween>
	) : (
		<Box color="text-status-inactive">Pending</Box>
	);
}

export function SelectField({
	label,
	value,
	options,
	onChange,
	placeholder = "Choose an option",
	description,
	empty = "No options available",
}: {
	label: string;
	value: string;
	options: string[];
	onChange: (value: string) => void;
	placeholder?: string;
	description?: string;
	empty?: string;
}) {
	return (
		<FormField label={label} description={description}>
			<Select
				ariaLabel={label}
				selectedOption={value ? { label: value, value } : null}
				options={options.map((v) => ({ label: v, value: v }))}
				placeholder={options.length ? placeholder : empty}
				empty={empty}
				onChange={({ detail }) => onChange(detail.selectedOption.value ?? "")}
			/>
		</FormField>
	);
}

export function MultiField({
	label,
	values,
	options,
	onChange,
	empty,
}: {
	label: string;
	values: string[];
	options: { value: string; label: string }[];
	onChange: (values: string[]) => void;
	empty: string;
}) {
	return (
		<FormField label={label}>
			<Multiselect
				ariaLabel={label}
				selectedOptions={options.filter((o) => values.includes(o.value))}
				options={options}
				placeholder={options.length ? "Choose options" : empty}
				empty={empty}
				onChange={({ detail }) =>
					onChange(detail.selectedOptions.map((o) => o.value ?? ""))
				}
			/>
		</FormField>
	);
}

type Ec2Item = Record<string, unknown>;

// useCacheOptions loads the lists that create and modify dialogs choose from.
export function useCacheOptions(scope: ElastiCacheScope, enabled: boolean) {
	const poll = usePoll(async () => {
		if (!enabled)
			return {
				versions: [] as EngineVersion[],
				subnetGroups: [] as SubnetGroup[],
				parameterGroups: [] as ParameterGroup[],
				subnets: [] as { value: string; label: string }[],
				securityGroups: [] as { value: string; label: string }[],
			};
		const ec2 = async (op: string, key: string) =>
			(
				await api.action<Record<string, Ec2Item[]>>(
					"ec2",
					op,
					scope.account,
					{},
					scope.region,
				)
			)[key] ?? [];
		const [versions, subnetGroups, parameterGroups, subnets, groups] =
			await Promise.all([
				ecPages<EngineVersion>(
					scope,
					"DescribeCacheEngineVersions",
					"CacheEngineVersions",
				),
				ecPages<SubnetGroup>(
					scope,
					"DescribeCacheSubnetGroups",
					"CacheSubnetGroups",
				),
				ecPages<ParameterGroup>(
					scope,
					"DescribeCacheParameterGroups",
					"CacheParameterGroups",
				),
				ec2("DescribeSubnets", "Subnets"),
				ec2("DescribeSecurityGroups", "SecurityGroups"),
			]);
		return {
			versions,
			subnetGroups,
			parameterGroups,
			subnets: subnets.map((s) => ({
				value: String(s.SubnetId),
				label: `${s.SubnetId} (${s.VpcId ?? "no VPC"})`,
			})),
			securityGroups: groups.map((g) => ({
				value: String(g.GroupId),
				label: `${g.GroupId} (${g.GroupName ?? ""})`,
			})),
		};
	}, 0);
	return poll;
}

export type CacheOptions = NonNullable<
	ReturnType<typeof useCacheOptions>["data"]
>;

// CacheList renders a selectable table with the standard toolbar, filter and pagination.
export function CacheList<T>({
	title,
	items,
	trackBy,
	columns,
	loading,
	error,
	emptyText,
	actions,
	onRefresh,
	selected,
	onSelect,
	detail,
}: {
	title: string;
	items: T[];
	trackBy: keyof T & string;
	columns: TableProps.ColumnDefinition<T>[];
	loading: boolean;
	error?: string;
	// emptyText tells the reader how to add the first item.
	emptyText: string;
	actions: ReactNode;
	onRefresh: () => void;
	selected?: T;
	onSelect: (item: T | undefined) => void;
	detail?: ReactNode;
}) {
	const {
		items: rows,
		collectionProps,
		filterProps,
		paginationProps,
	} = useCollection(items, {
		filtering: {
			empty: (
				<Box>
					No {title.toLowerCase()}. {emptyText}
				</Box>
			),
			noMatch: <Box>No matching {title.toLowerCase()}.</Box>,
		},
		sorting: { defaultState: { sortingColumn: { sortingField: trackBy } } },
		pagination: { pageSize: 20 },
	});
	return (
		<SpaceBetween size="l">
			{error && <Alert type="error">{error}</Alert>}
			<Table
				{...collectionProps}
				items={rows}
				trackBy={trackBy}
				selectionType="single"
				selectedItems={selected ? [selected] : []}
				onSelectionChange={({ detail }) => onSelect(detail.selectedItems[0])}
				loading={loading}
				loadingText={`Loading ${title.toLowerCase()}`}
				variant="full-page"
				columnDefinitions={columns}
				header={
					<Header
						variant="awsui-h1-sticky"
						counter={`(${items.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel={`Refresh ${title.toLowerCase()}`}
									onClick={onRefresh}
								/>
								{actions}
							</SpaceBetween>
						}
					>
						{title}
					</Header>
				}
				filter={
					<TextFilter
						{...filterProps}
						filteringPlaceholder={`Find ${title.toLowerCase()}`}
					/>
				}
				pagination={<Pagination {...paginationProps} />}
			/>
			{selected && detail}
		</SpaceBetween>
	);
}

// CacheTags lists and edits one resource's tags through the tagging operations.
export function CacheTags({
	scope,
	arn,
	enabled,
}: {
	scope: ElastiCacheScope;
	arn: string;
	enabled: boolean;
}) {
	const tags = usePoll(
		async () =>
			enabled
				? ((
						await ecCall<{ TagList?: Tag[] }>(scope, "ListTagsForResource", {
							ResourceName: arn,
						})
					).TagList ?? [])
				: [],
		0,
	);
	return (
		<ConsoleTags
			tags={Object.fromEntries((tags.data ?? []).map((t) => [t.Key, t.Value]))}
			enabled={enabled}
			onChanged={tags.reload}
			plan={(next, removed) => [
				...(removed.length
					? [
							{
								label: "Remove tags",
								run: () =>
									ecCall(scope, "RemoveTagsFromResource", {
										ResourceName: arn,
										TagKeys: removed,
									}),
							},
						]
					: []),
				...(Object.keys(next).length
					? [
							{
								label: "Save tags",
								run: () =>
									ecCall(scope, "AddTagsToResource", {
										ResourceName: arn,
										Tags: ecTags(next),
									}),
							},
						]
					: []),
			]}
		/>
	);
}

export function DetailBox({
	title,
	children,
	actions,
}: {
	title: string;
	children: ReactNode;
	actions?: ReactNode;
}) {
	return (
		<Container
			header={
				<Header variant="h2" actions={actions}>
					{title}
				</Header>
			}
		>
			{children}
		</Container>
	);
}
