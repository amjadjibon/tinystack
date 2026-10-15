import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	Flashbar,
	Header,
	KeyValuePairs,
	Link,
	Pagination,
	SpaceBetween,
	Table,
	Tabs,
	TextFilter,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleLayout } from "#/components/Console";
import { ConsoleTags } from "#/components/ConsoleTags";
import type { Resource } from "#/lib/api";
import {
	type CostService,
	configuration,
	costCall,
	costHref,
	period,
} from "#/lib/cost";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { BudgetForm } from "./BudgetForm";
import { BudgetNotifications } from "./BudgetNotifications";
import { ExplorerForm } from "./ExplorerForm";
import { ReportForm } from "./ReportForm";

export const costSections = [
	{ id: "anomalymonitor", label: "Anomaly monitors" },
	{ id: "anomalysubscription", label: "Anomaly subscriptions" },
	{ id: "costcategory", label: "Cost categories" },
];
export function CostPage({
	service,
	section,
	name,
}: {
	service: CostService;
	section: string;
	name: string;
}) {
	const { account, inventory, capabilities, canWrite } = useScope();
	const title =
		service === "budgets"
			? "AWS Budgets"
			: service === "costexplorer"
				? "AWS Cost Explorer"
				: "Cost and Usage Reports";
	const href = costHref(service, account, section);
	const displayName =
		inventory.data?.find(
			(r) =>
				r.service === service &&
				r.account === account &&
				(service === "costexplorer" ? r.arn === name : r.name === name),
		)?.name ?? name;
	const available = !!capabilities.data?.[service];
	const enabled = available && canWrite && /^\d{12}$/.test(account);
	return (
		<ConsoleLayout
			contentType={name ? "default" : "table"}
			navigationHeader={{ text: title, href }}
			activeHref={href}
			navigationItems={[
				...(service === "costexplorer"
					? costSections.map((s) => ({
							type: "link" as const,
							text: s.label,
							href: costHref(service, account, s.id),
						}))
					: [
							{
								type: "link" as const,
								text: service === "budgets" ? "Budgets" : "Report definitions",
								href,
							},
						]),
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: title, href },
				...(name
					? [
							{
								text: displayName,
								href: costHref(service, account, section, name),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					<Alert type="info">
						{service === "budgets"
							? "TinyStack stores budget, notification and subscriber definitions. It does not calculate spend or deliver alerts."
							: service === "costexplorer"
								? "TinyStack stores anomaly monitor, subscription and cost category definitions. It does not evaluate these definitions or deliver alerts."
								: "TinyStack stores report definitions. It does not calculate costs or deliver report files to S3."}
					</Alert>
					{(inventory.error || capabilities.error) && (
						<Alert type="error" header="Cannot load definitions">
							{inventory.error || capabilities.error}
						</Alert>
					)}
					{capabilities.data && !available && (
						<Alert type="warning">
							This service is not enabled for console management on this server.
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="warning">
							Management is read-only. Connect a console session for this
							account using Console session in the top bar.
						</Alert>
					)}
					<CostRecords
						key={`${service}/${account}/${section}/${name}/${enabled}`}
						service={service}
						account={account}
						section={section}
						name={name}
						enabled={enabled}
						resources={(inventory.data ?? []).filter(
							(r) =>
								r.service === service &&
								r.account === account &&
								r.type === section,
						)}
						loading={inventory.loading}
						reload={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}
function CostRecords({
	service,
	account,
	section,
	name,
	enabled,
	resources,
	loading,
	reload,
}: {
	service: CostService;
	account: string;
	section: string;
	name: string;
	enabled: boolean;
	resources: Resource[];
	loading: boolean;
	reload: () => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow();
	const [dialog, setDialog] = useState<"create" | "edit" | "delete">();
	const [notice, setNotice] = useState("");
	const mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	const changed = () => {
		reload();
		if (mounted.current) {
			setDialog(undefined);
			setNotice("Definition saved.");
		}
	};
	const go = (nextSection = section, nextName = "") =>
		navigate({ href: costHref(service, account, nextSection, nextName) });
	const selected = resources.find((r) =>
		service === "costexplorer" ? r.arn === name : r.name === name,
	);
	const config = selected ? configuration(selected) : {};
	const label =
		service === "budgets"
			? "Budgets"
			: service === "costandusagereportservice"
				? "Report definitions"
				: (costSections.find((s) => s.id === section)?.label ??
					"Anomaly monitors");
	const noun =
		service === "budgets"
			? "budget"
			: service === "costandusagereportservice"
				? "report definition"
				: section === "costcategory"
					? "cost category"
					: section === "anomalymonitor"
						? "anomaly monitor"
						: "anomaly subscription";
	const collection = useCollection(resources, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
	});
	const call = (operation: string, input: Record<string, unknown>) =>
		costCall(service, account, operation, input);
	return (
		<SpaceBetween size="l">
			{notice && (
				<Flashbar
					items={[
						{
							id: "cost-success",
							type: "success",
							content: notice,
							dismissible: true,
							onDismiss: () => setNotice(""),
						},
					]}
				/>
			)}
			{service === "costexplorer" && (
				<Tabs
					activeTabId={section}
					onChange={({ detail }) => go(detail.activeTabId)}
					tabs={costSections.map((s) => ({ id: s.id, label: s.label }))}
				/>
			)}
			<ContentLayout
				header={
					<Header
						variant="h1"
						description={`Account-wide definitions for ${account}.`}
						actions={
							name &&
							selected && (
								<SpaceBetween direction="horizontal" size="xs">
									<Button onClick={() => go()}>Back to list</Button>
									<Button
										disabled={!enabled}
										onClick={() => setDialog("delete")}
									>
										Delete
									</Button>
									<Button disabled={!enabled} onClick={() => setDialog("edit")}>
										{service === "budgets" ? "Edit limit" : "Edit"}
									</Button>
								</SpaceBetween>
							)
						}
					>
						{selected?.name || name || label}
					</Header>
				}
			>
				{name ? (
					selected ? (
						<SpaceBetween size="l">
							<Container header={<Header variant="h2">Definition</Header>}>
								<KeyValuePairs
									columns={2}
									items={[
										{ label: "Name", value: selected.name },
										{ label: "ARN", value: selected.arn },
										{ label: "Account", value: account },
										{ label: "Scope", value: "Global" },
									]}
								/>
								<CodeBlock text={JSON.stringify(config, null, 2)} />
							</Container>
							{service === "budgets" && (
								<BudgetNotifications
									account={account}
									resource={selected}
									enabled={enabled}
									onChanged={changed}
								/>
							)}
							<ConsoleTags
								tags={selected.tags ?? {}}
								enabled={enabled}
								onChanged={changed}
								plan={(next, removed) => {
									const budget = service === "budgets";
									const cur = service === "costandusagereportservice";
									const arn = budget
										? { ResourceARN: selected.arn }
										: cur
											? { ReportName: selected.name }
											: { ResourceArn: selected.arn };
									const Tags = Object.entries(next).map(([Key, Value]) => ({
										Key,
										Value,
									}));
									return [
										...(removed.length
											? [
													{
														label: "Remove tags",
														run: () =>
															call("UntagResource", {
																...arn,
																...(!cur
																	? { ResourceTagKeys: removed }
																	: { TagKeys: removed }),
															}),
													},
												]
											: []),
										...(Tags.length
											? [
													{
														label: "Save tags",
														run: () =>
															call("TagResource", {
																...arn,
																...(!cur ? { ResourceTags: Tags } : { Tags }),
															}),
													},
												]
											: []),
									];
								}}
							/>
						</SpaceBetween>
					) : (
						<Box>
							{loading
								? "Loading definition"
								: "Definition not found in this account. Refresh the list or select another definition."}
							<Button onClick={() => go()}>Back to list</Button>
						</Box>
					)
				) : (
					<Table
						{...collection.collectionProps}
						items={collection.items}
						trackBy="arn"
						loading={loading}
						loadingText="Loading definitions"
						header={
							<Header
								counter={`(${resources.length})`}
								actions={
									<SpaceBetween direction="horizontal" size="xs">
										<Button
											iconName="refresh"
											ariaLabel="Refresh definitions"
											onClick={reload}
										/>
										<Button
											variant="primary"
											disabled={!enabled}
											onClick={() => setDialog("create")}
										>
											Create {noun}
										</Button>
									</SpaceBetween>
								}
							>
								{label}
							</Header>
						}
						filter={
							<TextFilter
								{...collection.filterProps}
								filteringAriaLabel="Find definitions"
								filteringPlaceholder="Find definitions"
							/>
						}
						pagination={<Pagination {...collection.paginationProps} />}
						columnDefinitions={[
							{
								id: "name",
								header: "Name",
								sortingField: "name",
								cell: (r) => (
									<Link
										href={costHref(
											service,
											account,
											section,
											service === "costexplorer" ? r.arn : r.name,
										)}
										onFollow={follow}
									>
										{r.name}
									</Link>
								),
							},
							...(service === "budgets"
								? [
										{
											id: "type",
											header: "Type",
											cell: (r: Resource) =>
												String(configuration(r).BudgetType ?? "—"),
										},
										{
											id: "time",
											header: "Time unit",
											cell: (r: Resource) =>
												String(configuration(r).TimeUnit ?? "—"),
										},
										{
											id: "limit",
											header: "Limit",
											cell: (r: Resource) => {
												const limit = configuration(r).BudgetLimit as
													| { Amount: string; Unit: string }
													| undefined;
												return limit
													? `${limit.Amount} ${limit.Unit}`
													: "Planned limits";
											},
										},
										{
											id: "period",
											header: "Period",
											cell: (r: Resource) =>
												period(configuration(r).TimePeriod),
										},
									]
								: service === "costandusagereportservice"
									? [
											{
												id: "time",
												header: "Time unit",
												cell: (r: Resource) =>
													String(configuration(r).TimeUnit ?? "—"),
											},
											{
												id: "format",
												header: "Format",
												cell: (r: Resource) =>
													String(configuration(r).Format ?? "—"),
											},
											{
												id: "bucket",
												header: "S3 bucket",
												cell: (r: Resource) =>
													String(configuration(r).S3Bucket ?? "—"),
											},
										]
									: [
											{
												id: "type",
												header:
													section === "anomalymonitor"
														? "Monitor type"
														: section === "anomalysubscription"
															? "Frequency"
															: "Rule version",
												cell: (r: Resource) =>
													String(
														configuration(r)[
															section === "anomalymonitor"
																? "MonitorType"
																: section === "anomalysubscription"
																	? "Frequency"
																	: "RuleVersion"
														] ?? "—",
													),
											},
										]),
							{ id: "arn", header: "ARN", cell: (r) => r.arn },
						]}
						empty={
							<Box textAlign="center" padding="l">
								{collection.filterProps.filteringText
									? "No matching definitions"
									: `No ${label.toLowerCase()}`}
							</Box>
						}
					/>
				)}
			</ContentLayout>
			{(dialog === "create" || (dialog === "edit" && selected)) &&
				(service === "budgets" ? (
					<BudgetForm
						account={account}
						resource={dialog === "edit" ? selected : undefined}
						onClose={() => setDialog(undefined)}
						onSaved={changed}
					/>
				) : service === "costexplorer" ? (
					<ExplorerForm
						account={account}
						section={section}
						resource={dialog === "edit" ? selected : undefined}
						onClose={() => setDialog(undefined)}
						onSaved={changed}
					/>
				) : (
					<ReportForm
						account={account}
						resource={dialog === "edit" ? selected : undefined}
						onClose={() => setDialog(undefined)}
						onSaved={changed}
					/>
				))}
			{dialog === "delete" && selected && (
				<ActionDialog
					title={`Delete ${noun}`}
					submit="Delete"
					confirmation={selected.name}
					onClose={() => setDialog(undefined)}
					onSaved={() => {
						reload();
						if (mounted.current) go();
					}}
					plan={async () => {
						const operation =
							service === "budgets"
								? "DeleteBudget"
								: service === "costandusagereportservice"
									? "DeleteReportDefinition"
									: section === "anomalymonitor"
										? "DeleteAnomalyMonitor"
										: section === "anomalysubscription"
											? "DeleteAnomalySubscription"
											: "DeleteCostCategoryDefinition";
						const input =
							service === "budgets"
								? { AccountId: account, BudgetName: selected.name }
								: service === "costandusagereportservice"
									? { ReportName: selected.name }
									: {
											[section === "anomalymonitor"
												? "MonitorArn"
												: section === "anomalysubscription"
													? "SubscriptionArn"
													: "CostCategoryArn"]: selected.arn,
										};
						return [
							{ label: `Delete ${noun}`, run: () => call(operation, input) },
						];
					}}
				>
					Delete {selected.name} and its stored definition?
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
