import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Flashbar,
	Header,
	Link,
	Pagination,
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
import { LambdaFunction } from "#/components/lambda/LambdaFunction";
import { LambdaFunctionForm } from "#/components/lambda/LambdaFunctionForm";
import type { Resource } from "#/lib/api";
import { functionConfig, type LambdaScope, lambdaHref } from "#/lib/lambda";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";

export const Route = createFileRoute("/services/lambda")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		function: String(search.function ?? ""),
		create: search.create === true || search.create === "true",
	}),
	component: LambdaPage,
});
function LambdaPage() {
	const search = Route.useSearch();
	const navigate = useNavigate();
	const location = useLocation();
	const { account, region, inventory, capabilities } = useScope();
	const scope = { account, region, functionName: search.function };
	const [notice, setNotice] = useState("");
	const context = `${account}/${region}/${search.function}/${search.create}/${location.pathname}`;
	const current = useRef(context);
	current.current = context;
	const mounted = useRef(true);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	// biome-ignore lint/correctness/useExhaustiveDependencies: notices belong to the selected account and Region.
	useEffect(() => setNotice(""), [account, region]);
	const actionable =
		!capabilities.data?.requires_signature &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const enabled = !!capabilities.data?.lambda && actionable;
	const functions = (inventory.data ?? []).filter(
		(r) =>
			r.service === "lambda" &&
			r.type === "function" &&
			r.account === account &&
			r.region === region,
	);
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current && current.current === context) setNotice(message);
	};
	const go = (functionName = "", create = false) => {
		if (!mounted.current || current.current !== context) return;
		return navigate({
			to: "/services/lambda",
			search: { account, region, function: functionName, create },
		});
	};
	const listHref = lambdaHref({ account, region });
	return (
		<ConsoleLayout
			contentType={search.function || search.create ? "default" : "table"}
			navigationHeader={{ text: "AWS Lambda", href: listHref }}
			activeHref={listHref}
			navigationItems={[
				{ type: "link", text: "Functions", href: listHref },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "AWS Lambda", href: listHref },
				{ text: "Functions", href: listHref },
				...(search.create
					? [
							{
								text: "Create function",
								href: lambdaHref({ account, region }, true),
							},
						]
					: search.function
						? [{ text: search.function, href: lambdaHref(scope) }]
						: []),
			]}
			content={
				<SpaceBetween size="l">
					{notice && (
						<Flashbar
							items={[
								{
									id: "lambda-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error" header="Cannot load Lambda">
							{inventory.error || capabilities.error}
						</Alert>
					)}
					{capabilities.data?.requires_signature && (
						<Alert type="info" header="Lambda management is read-only">
							This server requires signed requests. Function and trigger
							inventory is available here; use a signed AWS CLI or SDK request
							for management, invocation and log reads.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.lambda && (
						<Alert type="warning">Lambda is not enabled on this server.</Alert>
					)}
					{search.create ? (
						enabled ? (
							<LambdaFunctionForm
								key={`${account}/${region}`}
								scope={scope}
								resources={inventory.data ?? []}
								onCancel={() => go()}
								onSaved={(name) => {
									changed(`Function ${name} created.`);
									go(name);
								}}
							/>
						) : (
							<Box>
								Function creation is unavailable in the current server mode.
							</Box>
						)
					) : search.function ? (
						<LambdaFunction
							key={`${account}/${region}/${search.function}/${enabled}`}
							scope={scope}
							resource={functions.find((f) => f.name === search.function)}
							resources={inventory.data ?? []}
							enabled={enabled}
							snsEnabled={!!capabilities.data?.sns && actionable}
							logsEnabled={!!capabilities.data?.logs && actionable}
							loading={inventory.loading}
							onRefresh={inventory.reload}
							onChanged={changed}
							onDeleted={() => {
								changed(`Function ${search.function} deleted.`);
								go();
							}}
						/>
					) : (
						<FunctionList
							key={`${account}/${region}`}
							scope={scope}
							functions={functions}
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
function FunctionList({
	scope,
	functions,
	enabled,
	loading,
	onRefresh,
	onCreate,
}: {
	scope: LambdaScope;
	functions: Resource[];
	enabled: boolean;
	loading: boolean;
	onRefresh: () => void;
	onCreate: () => void;
}) {
	const follow = useFollow();
	const rows = functions.map((r) => ({ ...r, ...functionConfig(r) }));
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
					No functions. Create a function to deploy and test your code.
				</Box>
			),
			noMatch: (
				<Box textAlign="center" padding="l">
					No functions match your search.
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
			loadingText="Loading functions"
			header={
				<Header
					variant="awsui-h1-sticky"
					counter={`(${rows.length})`}
					description="Deploy and run functions in the selected account and Region."
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh functions"
								onClick={onRefresh}
							/>
							<Button variant="primary" disabled={!enabled} onClick={onCreate}>
								Create function
							</Button>
						</SpaceBetween>
					}
				>
					Functions
				</Header>
			}
			filter={
				<TextFilter
					{...filterProps}
					filteringPlaceholder="Find functions"
					countText={`${filteredItemsCount ?? 0} matches`}
				/>
			}
			pagination={<Pagination {...paginationProps} />}
			columnDefinitions={[
				{
					id: "name",
					header: "Function name",
					sortingField: "name",
					isRowHeader: true,
					cell: (f) => (
						<Link
							href={lambdaHref({ ...scope, functionName: f.name })}
							onFollow={follow}
						>
							{f.name}
						</Link>
					),
				},
				{
					id: "description",
					header: "Description",
					cell: (f) => f.Description || "—",
				},
				{
					id: "runtime",
					header: "Runtime",
					cell: (f) => f.Runtime ?? "Container image",
					sortingField: "Runtime",
				},
				{
					id: "package",
					header: "Package type",
					cell: (f) => f.PackageType,
					sortingField: "PackageType",
				},
				{
					id: "memory",
					header: "Memory",
					cell: (f) => `${f.MemorySize} MB`,
					sortingField: "MemorySize",
				},
				{
					id: "modified",
					header: "Last modified",
					cell: (f) => f.LastModified,
					sortingField: "LastModified",
				},
			]}
		/>
	);
}
