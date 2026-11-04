import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Header,
	Link,
	Pagination,
	Select,
	SpaceBetween,
	Table,
	TextFilter,
	Toggle,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { StackDetails } from "#/components/cloudformation/StackDetails";
import { StackEditor } from "#/components/cloudformation/StackEditor";
import { StackExports } from "#/components/cloudformation/StackExports";
import { StackStatus } from "#/components/cloudformation/StackStatus";
import { type CfnScope, cfnHref, cfnPages } from "#/lib/cloudformation";
import {
	cfnDate,
	filterStacks,
	type StackSummary,
	stackNameOf,
} from "#/lib/cloudformationHelpers";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/cloudformation")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		stack: String(s.stack ?? ""),
		changeset: String(s.changeset ?? ""),
	}),
	component: CloudFormationPage,
});

function CloudFormationPage() {
	const search = Route.useSearch(),
		{ canWrite, account, region, capabilities, inventory } = useScope();
	const scope: CfnScope = { account, region, stack: search.stack || undefined };
	const enabled =
		!!capabilities.data?.pages.cloudformation &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const list = cfnHref({ account, region });
	return (
		<ConsoleLayout
			contentType={search.stack ? "default" : "table"}
			navigationHeader={{ text: "CloudFormation", href: list }}
			activeHref={list}
			navigationItems={[
				{ type: "link", text: "Stacks", href: list },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "CloudFormation", href: list },
				{ text: "Stacks", href: list },
				...(search.stack
					? [{ text: stackNameOf(search.stack), href: cfnHref(scope) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!capabilities.error && (
						<Alert type="error">{capabilities.error}</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.cloudformation && (
						<Alert type="warning">
							CloudFormation is not enabled on this server.
						</Alert>
					)}
					{search.stack && enabled ? (
						<StackDetails
							key={`${account}/${region}/${search.stack}`}
							scope={scope}
							inventory={inventory.data ?? []}
							initialChangeSet={search.changeset || undefined}
							onChanged={inventory.reload}
						/>
					) : (
						<StackList
							key={`${account}/${region}/${enabled}`}
							scope={scope}
							enabled={enabled}
							fallback={(inventory.data ?? [])
								.filter(
									(r) =>
										r.service === "cloudformation" &&
										r.type === "stack" &&
										r.account === account &&
										r.region === region,
								)
								.map((r) => ({
									StackId: r.arn,
									StackName: r.name,
									StackStatus: String(r.details?.Status ?? ""),
								}))}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}

// StackList reads ListStacks, which also returns deleted stacks; they stay
// hidden until "Include deleted" is on or the status filter asks for them.
function StackList({
	scope,
	enabled,
	fallback,
}: {
	scope: CfnScope;
	enabled: boolean;
	fallback: StackSummary[];
}) {
	const follow = useFollow(),
		[status, setStatus] = useState(""),
		[includeDeleted, setIncludeDeleted] = useState(false);
	const poll = usePoll(() =>
		enabled
			? cfnPages<StackSummary>(scope, "ListStacks", "StackSummaries")
			: Promise.resolve(undefined),
	);
	const stacks = poll.data ?? fallback;
	const visible = filterStacks(stacks, status, includeDeleted);
	const statuses = [...new Set(stacks.map((s) => s.StackStatus))].sort();
	const hidden = !status && !includeDeleted && stacks.length > visible.length;
	const {
		items,
		collectionProps,
		filterProps,
		paginationProps,
		filteredItemsCount,
	} = useCollection(visible, {
		filtering: {
			empty: (
				<Box textAlign="center" padding="l">
					{hidden
						? "No stacks. Deleted stacks are hidden; turn on Include deleted to see them."
						: status
							? `No stacks with status ${status}.`
							: "No stacks in this account and Region."}
				</Box>
			),
			noMatch: (
				<Box textAlign="center" padding="l">
					No stacks match your search.
				</Box>
			),
		},
		sorting: {
			defaultState: {
				sortingColumn: { sortingField: "CreationTime" },
				isDescending: true,
			},
		},
		pagination: { pageSize: 20 },
	});
	return (
		<SpaceBetween size="l">
			{!!poll.error && (
				<Alert type="error" header="Cannot load stacks">
					{poll.error}
				</Alert>
			)}
			<Table
				{...collectionProps}
				items={items}
				trackBy="StackId"
				loading={poll.loading && enabled}
				loadingText="Loading stacks"
				variant="full-page"
				header={
					<Header
						variant="awsui-h1-sticky"
						counter={`(${visible.length})`}
						description="Create and manage stacks in the selected account and Region."
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<StackEditor
									scope={scope}
									disabled={!enabled}
									onSaved={(id, changeSet) => {
										poll.reload();
										if (id)
											window.location.assign(
												cfnHref({ ...scope, stack: id }, changeSet),
											);
									}}
								/>
								<Button
									iconName="refresh"
									ariaLabel="Refresh stacks"
									onClick={poll.reload}
								/>
							</SpaceBetween>
						}
					>
						Stacks
					</Header>
				}
				filter={
					<SpaceBetween direction="horizontal" size="s" alignItems="center">
						<TextFilter
							{...filterProps}
							filteringPlaceholder="Find stacks"
							countText={`${filteredItemsCount ?? 0} matches`}
						/>
						<Select
							ariaLabel="Filter by status"
							selectedOption={
								status
									? { value: status, label: status }
									: { value: "", label: "All statuses" }
							}
							options={[
								{ value: "", label: "All statuses" },
								...statuses.map((value) => ({ value, label: value })),
							]}
							onChange={({ detail }) =>
								setStatus(detail.selectedOption.value ?? "")
							}
						/>
						<Toggle
							checked={includeDeleted}
							onChange={({ detail }) => setIncludeDeleted(detail.checked)}
						>
							Include deleted
						</Toggle>
					</SpaceBetween>
				}
				pagination={<Pagination {...paginationProps} />}
				columnDefinitions={[
					{
						id: "name",
						header: "Stack name",
						sortingField: "StackName",
						isRowHeader: true,
						cell: (s) => (
							<Link
								href={cfnHref({ ...scope, stack: s.StackId })}
								onFollow={follow}
							>
								{s.StackName}
							</Link>
						),
					},
					{
						id: "status",
						header: "Status",
						sortingField: "StackStatus",
						cell: (s) => <StackStatus status={s.StackStatus} />,
					},
					{
						id: "created",
						header: "Created",
						sortingField: "CreationTime",
						cell: (s) => cfnDate(s.CreationTime),
					},
					{
						id: "updated",
						header: "Updated",
						sortingField: "LastUpdatedTime",
						cell: (s) => cfnDate(s.LastUpdatedTime),
					},
					{
						id: "drift",
						header: "Drift status",
						cell: (s) => s.DriftInformation?.StackDriftStatus ?? "—",
					},
					{
						id: "description",
						header: "Description",
						cell: (s) => s.TemplateDescription ?? "—",
					},
				]}
			/>
			{enabled && <StackExports scope={scope} />}
		</SpaceBetween>
	);
}
