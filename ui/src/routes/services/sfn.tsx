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
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { ExecutionDetails } from "#/components/sfn/ExecutionDetails";
import { StateMachineDetails } from "#/components/sfn/StateMachineDetails";
import { StateMachineForm } from "#/components/sfn/StateMachineForm";
import type { Resource } from "#/lib/api";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { type SfnScope, sfnHref } from "#/lib/sfn";
import { useFollow } from "#/lib/useFollow";

export const Route = createFileRoute("/services/sfn")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		machine: String(search.machine ?? ""),
		execution: search.machine ? String(search.execution ?? "") : "",
		create: search.create === true || search.create === "true",
	}),
	component: StepFunctionsPage,
});
function StepFunctionsPage() {
	const search = Route.useSearch(),
		navigate = useNavigate();
	const { canWrite, account, region, inventory, capabilities } = useScope();
	const scope = {
		account,
		region,
		machine: search.machine,
		execution: search.execution,
	};
	const [notice, setNotice] = useState("");
	const context = `${account}/${region}/${search.machine}/${search.execution}/${search.create}`;
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
	const enabled =
		!!capabilities.data?.pages.sfn &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const machines = (inventory.data ?? []).filter(
		(r) =>
			r.service === "sfn" &&
			r.type === "state-machine" &&
			r.account === account &&
			r.region === region,
	);
	const listHref = sfnHref({ account, region });
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current && current.current === context) setNotice(message);
	};
	const go = (machine = "", execution = "", create = false) => {
		if (!mounted.current || current.current !== context) return;
		return navigate({
			to: "/services/sfn",
			search: { account, region, machine, execution, create },
		});
	};
	return (
		<ConsoleLayout
			contentType={search.machine ? "default" : "table"}
			navigationHeader={{ text: "Step Functions", href: listHref }}
			activeHref={listHref}
			navigationItems={[
				{ type: "link", text: "State machines", href: listHref },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Step Functions", href: listHref },
				{ text: "State machines", href: listHref },
				...(search.machine
					? [
							{
								text: search.machine,
								href: sfnHref({ ...scope, execution: undefined }),
							},
						]
					: []),
				...(search.execution
					? [{ text: search.execution, href: sfnHref(scope) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{notice && (
						<Flashbar
							items={[
								{
									id: "sfn-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error" header="Cannot load Step Functions">
							{inventory.error || capabilities.error}
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info" header="Step Functions management is read-only">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.sfn && (
						<Alert type="warning">
							Step Functions is not enabled on this server.
						</Alert>
					)}
					{search.execution ? (
						enabled ? (
							<ExecutionDetails
								key={`${context}/${enabled}`}
								scope={scope}
								onChanged={changed}
							/>
						) : (
							<Box>Execution reads are unavailable in this server mode.</Box>
						)
					) : search.machine ? (
						<StateMachineDetails
							key={`${context}/${enabled}`}
							scope={scope}
							resource={machines.find((r) => r.name === search.machine)}
							resources={inventory.data ?? []}
							enabled={enabled}
							onChanged={changed}
							onDeleted={() => {
								changed(`State machine ${search.machine} deleted.`);
								go();
							}}
							onExecution={(name) => {
								changed(`Execution ${name} started.`);
								go(search.machine, name);
							}}
						/>
					) : (
						<StateMachineList
							key={`${account}/${region}`}
							scope={scope}
							machines={machines}
							enabled={enabled}
							loading={inventory.loading}
							onRefresh={inventory.reload}
							onCreate={() => go("", "", true)}
						/>
					)}
					{search.create && enabled && (
						<StateMachineForm
							key={`${context}/${enabled}`}
							scope={scope}
							resources={inventory.data ?? []}
							onClose={() => go()}
							onSaved={(name) => {
								changed(`State machine ${name} created.`);
								go(name);
							}}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}
function StateMachineList({
	scope,
	machines,
	enabled,
	loading,
	onRefresh,
	onCreate,
}: {
	scope: SfnScope;
	machines: Resource[];
	enabled: boolean;
	loading: boolean;
	onRefresh: () => void;
	onCreate: () => void;
}) {
	const follow = useFollow();
	const rows = machines.map((r) => ({
		...r,
		kind: String(r.details?.Type ?? "STANDARD"),
		role: String(r.details?.RoleArn ?? ""),
	}));
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
					No state machines. Create a workflow to get started.
				</Box>
			),
			noMatch: (
				<Box textAlign="center" padding="l">
					No state machines match your search.
				</Box>
			),
		},
		sorting: { defaultState: { sortingColumn: { sortingField: "name" } } },
		pagination: { pageSize: 20 },
	});
	return (
		<Table
			{...collectionProps}
			items={items}
			loading={loading}
			loadingText="Loading state machines"
			trackBy="arn"
			header={
				<Header
					variant="h1"
					counter={`(${machines.length})`}
					description="Orchestrate tasks with Amazon States Language workflows."
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh state machines"
								onClick={onRefresh}
							/>
							<Button variant="primary" disabled={!enabled} onClick={onCreate}>
								Create state machine
							</Button>
						</SpaceBetween>
					}
				>
					State machines
				</Header>
			}
			filter={
				<TextFilter
					{...filterProps}
					filteringPlaceholder="Find state machines"
					countText={`${filteredItemsCount ?? 0} matches`}
				/>
			}
			pagination={<Pagination {...paginationProps} />}
			columnDefinitions={[
				{
					id: "name",
					header: "Name",
					sortingField: "name",
					cell: (r) => (
						<Link
							href={sfnHref({ ...scope, machine: r.name })}
							onFollow={follow}
						>
							{r.name}
						</Link>
					),
				},
				{
					id: "type",
					header: "Type",
					sortingField: "kind",
					cell: (r) => r.kind,
				},
				{ id: "role", header: "Execution role", cell: (r) => r.role || "—" },
				{ id: "arn", header: "State machine ARN", cell: (r) => r.arn },
			]}
		/>
	);
}
