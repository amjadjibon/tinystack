import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	FormField,
	Header,
	Hotspot,
	Input,
	Link,
	Pagination,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleLayout } from "#/components/Console";
import { ParameterDetails } from "#/components/ssm/ParameterDetails";
import {
	Choice,
	ParameterForm,
	parameterTypes,
} from "#/components/ssm/ParameterForm";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import {
	type Parameter,
	type SSMScope,
	ssmCall,
	ssmHref,
	ssmPages,
} from "#/lib/ssm";
import { parameterFilters } from "#/lib/ssmHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { HOTSPOT } from "#/tutorials/hotspots";
export const Route = createFileRoute("/services/ssm")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		parameter: String(s.parameter ?? ""),
	}),
	component: SSMPage,
});
function SSMPage() {
	const search = Route.useSearch(),
		{ account, region, canWrite, capabilities, inventory } = useScope();
	const scope = { account, region, parameter: search.parameter },
		list = ssmHref({ account, region });
	const enabled =
		!!capabilities.data?.pages.ssm &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	return (
		<ConsoleLayout
			contentType={search.parameter ? "default" : "table"}
			navigationHeader={{ text: "Systems Manager", href: list }}
			activeHref={list}
			navigationItems={[
				{ type: "link", text: "Parameter Store", href: list },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "Parameter Store", href: list },
				...(search.parameter
					? [{ text: search.parameter, href: ssmHref(scope) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!capabilities.error && (
						<Alert type="error">{capabilities.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.ssm && (
						<Alert type="warning">
							Systems Manager is not enabled on this server.
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root console session for the selected account to manage
							parameters.
						</Alert>
					)}
					<Workspace
						key={`${account}/${region}/${search.parameter}/${enabled}`}
						scope={scope}
						enabled={enabled}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}
function Workspace({
	scope,
	enabled,
	onChanged,
}: {
	scope: SSMScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const navigate = useNavigate();
	const [create, setCreate] = useState(false),
		[revision, setRevision] = useState(0);
	const changed = () => {
		onChanged();
		setRevision((r) => r + 1);
	};
	return (
		<>
			{scope.parameter ? (
				<ParameterDetails
					scope={scope}
					enabled={enabled}
					onChanged={onChanged}
					onDeleted={() => {
						onChanged();
						navigate({
							href: ssmHref({ account: scope.account, region: scope.region }),
						});
					}}
				/>
			) : (
				<ParameterList
					key={revision}
					scope={scope}
					enabled={enabled}
					onCreate={() => setCreate(true)}
					onChanged={changed}
				/>
			)}
			{create && (
				<ParameterForm
					scope={scope}
					onClose={() => setCreate(false)}
					onSaved={(name) => {
						setCreate(false);
						changed();
						navigate({ href: ssmHref({ ...scope, parameter: name }) });
					}}
				/>
			)}
		</>
	);
}
function ParameterList({
	scope,
	enabled,
	onCreate,
	onChanged,
}: {
	scope: SSMScope;
	enabled: boolean;
	onCreate: () => void;
	onChanged: () => void;
}) {
	const [prefix, setPrefix] = useState(""),
		[mode, setMode] = useState("prefix"),
		[type, setType] = useState(""),
		[tier, setTier] = useState(""),
		[filter, setFilter] = useState({
			prefix: "",
			mode: "prefix",
			type: "",
			tier: "",
		});
	return (
		<FilteredList
			key={JSON.stringify(filter)}
			{...{ scope, enabled, onCreate, onChanged, filter }}
			filterBar={
				<SpaceBetween direction="horizontal" size="s" alignItems="end">
					<FormField label="Name prefix or path">
						<Input
							ariaLabel="Name prefix or path"
							value={prefix}
							onChange={({ detail }) => setPrefix(detail.value)}
						/>
					</FormField>
					<Choice
						label="Filter mode"
						value={mode}
						options={["prefix", "OneLevel", "Recursive"]}
						onChange={setMode}
					/>
					<Choice
						label="Type"
						value={type}
						options={["", ...parameterTypes]}
						onChange={setType}
					/>
					<Choice
						label="Tier"
						value={tier}
						options={["", "Standard", "Advanced"]}
						onChange={setTier}
					/>
					<Button onClick={() => setFilter({ prefix, mode, type, tier })}>
						Apply filters
					</Button>
				</SpaceBetween>
			}
		/>
	);
}
function FilteredList({
	scope,
	enabled,
	onCreate,
	onChanged,
	filter,
	filterBar,
}: {
	scope: SSMScope;
	enabled: boolean;
	onCreate: () => void;
	onChanged: () => void;
	filter: { prefix: string; mode: string; type: string; tier: string };
	/** filterBar holds the server-side filter controls, shown in the table's filter slot. */
	filterBar: ReactNode;
}) {
	const [selected, setSelected] = useState<Parameter[]>([]),
		[deleting, setDeleting] = useState(false);
	const follow = useFollow();
	const poll = usePoll(() =>
		enabled
			? ssmPages(scope, "DescribeParameters", {
					ParameterFilters: parameterFilters(
						filter.prefix,
						filter.mode,
						filter.type,
						filter.tier,
					),
				})
			: Promise.resolve([]),
	);
	const parameters = poll.data ?? [];
	const current = selected.filter((p) =>
		parameters.some((row) => row.Name === p.Name),
	);
	const { items, collectionProps, paginationProps } = useCollection(
		parameters,
		{
			sorting: { defaultState: { sortingColumn: { sortingField: "Name" } } },
			pagination: { pageSize: 20 },
		},
	);
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				{...collectionProps}
				items={items}
				trackBy="Name"
				selectionType="multi"
				selectedItems={current}
				onSelectionChange={({ detail }) => setSelected(detail.selectedItems)}
				loading={poll.loading}
				variant="full-page"
				empty={
					<Box>
						No parameters match. Create a parameter or adjust the path and type
						filters.
					</Box>
				}
				filter={filterBar}
				pagination={<Pagination {...paginationProps} />}
				header={
					<Header
						variant="h1"
						counter={`(${parameters.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button onClick={poll.reload}>Refresh</Button>
								<Button
									disabled={!enabled || !current.length}
									onClick={() => setDeleting(true)}
								>
									Delete parameters
								</Button>
								<Hotspot hotspotId={HOTSPOT.parameterCreate}>
									<Button
										variant="primary"
										disabled={!enabled}
										onClick={onCreate}
									>
										Create parameter
									</Button>
								</Hotspot>
							</SpaceBetween>
						}
					>
						Parameter Store
					</Header>
				}
				columnDefinitions={[
					{
						id: "name",
						header: "Name",
						sortingField: "Name",
						cell: (p) => (
							<Link
								href={ssmHref({ ...scope, parameter: p.Name })}
								onFollow={follow}
							>
								{p.Name}
							</Link>
						),
					},
					{
						id: "type",
						header: "Type",
						sortingField: "Type",
						cell: (p) => p.Type,
					},
					{
						id: "tier",
						header: "Tier",
						sortingField: "Tier",
						cell: (p) => p.Tier ?? "—",
					},
					{
						id: "version",
						header: "Version",
						sortingField: "Version",
						cell: (p) => p.Version,
					},
					{
						id: "description",
						header: "Description",
						cell: (p) => p.Description || "—",
					},
				]}
			/>
			{deleting && (
				<ActionDialog
					title="Delete parameters"
					submit="Delete parameters"
					confirmation="delete"
					onClose={() => setDeleting(false)}
					onSaved={() => {
						setDeleting(false);
						setSelected([]);
						poll.reload();
						onChanged();
					}}
					plan={async () => {
						const steps = [];
						for (let i = 0; i < current.length; i += 10) {
							const names = current.slice(i, i + 10).map((p) => p.Name);
							steps.push({
								label: `Delete ${names.join(", ")}`,
								run: async () => {
									// Missing parameters already satisfy the requested deletion.
									await ssmCall(scope, "DeleteParameters", { Names: names });
								},
							});
						}
						return steps;
					}}
				>
					<Box>
						Delete these parameters and all their versions permanently:{" "}
						{current.map((p) => p.Name).join(", ")}.
					</Box>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
