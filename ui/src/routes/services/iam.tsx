import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Autosuggest,
	Badge,
	Box,
	Button,
	ColumnLayout,
	Container,
	ContentLayout,
	Flashbar,
	FormField,
	Header,
	KeyValuePairs,
	Link,
	Pagination,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { EmptyState } from "#/components/EmptyState";
import { IAMCreate } from "#/components/iam/IAMCreate";
import { IAMDetail } from "#/components/iam/IAMDetail";
import { api, type Resource } from "#/lib/api";
import {
	type IAMKind,
	type IAMSection,
	iamHref,
	iamKinds,
	iamLabels,
} from "#/lib/iam";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

type IAMSearch = {
	section: IAMSection;
	account: string;
	name: string;
	create: boolean;
};
export const Route = createFileRoute("/services/iam")({
	validateSearch: (search: Record<string, unknown>): IAMSearch => ({
		section: iamKinds.includes(search.section as IAMKind)
			? (search.section as IAMKind)
			: "overview",
		account: String(search.account ?? ""),
		name: String(search.name ?? ""),
		create: search.create === true || search.create === "true",
	}),
	component: IAMPage,
});

function IAMPage() {
	const search = Route.useSearch();
	const navigate = useNavigate();
	const inventory = usePoll(api.resources);
	const capabilities = usePoll(api.actionCapabilities, 15000);
	const account = search.account || capabilities.data?.account || "";
	const [accountDraft, setAccountDraft] = useState(account);
	const [notice, setNotice] = useState("");
	useEffect(() => {
		setAccountDraft(account);
		setNotice("");
	}, [account]);
	const resources = (inventory.data ?? []).filter(
		(r) => r.service === "iam" && r.account === account,
	);
	const enabled =
		!!capabilities.data?.iam &&
		!capabilities.data.requires_signature &&
		/^\d{12}$/.test(account);
	const accounts = [
		...new Set([
			capabilities.data?.account ?? "",
			...(inventory.data ?? []).map((r) => r.account),
		]),
	]
		.filter(Boolean)
		.sort();
	const go = (
		section: IAMSection,
		name = "",
		create = false,
		nextAccount = account,
	) =>
		navigate({
			to: "/services/iam",
			search: { section, name, create, account: nextAccount },
		});
	const selected = resources.find(
		(r) => r.type === search.section && r.name === search.name,
	);
	const title =
		search.section === "overview"
			? "IAM dashboard"
			: iamLabels[search.section].plural;
	const refresh = () => inventory.reload();
	return (
		<ConsoleLayout
			contentType={
				search.name || search.create || search.section === "overview"
					? "default"
					: "table"
			}
			navigationHeader={{
				text: "Identity and Access Management (IAM)",
				href: iamHref("overview", account),
			}}
			activeHref={iamHref(search.section, account)}
			navigationItems={[
				{ type: "link", text: "Dashboard", href: iamHref("overview", account) },
				{
					type: "section",
					text: "Access management",
					items: iamKinds.map((kind) => ({
						type: "link",
						text: iamLabels[kind].plural,
						href: iamHref(kind, account),
					})),
				},
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "IAM", href: iamHref("overview", account) },
				...(search.section !== "overview"
					? [{ text: title, href: iamHref(search.section, account) }]
					: []),
				...(search.create
					? [{ text: "Create", href: "#" }]
					: search.name
						? [{ text: search.name, href: "#" }]
						: []),
			]}
			content={
				<SpaceBetween size="l">
					<SpaceBetween direction="horizontal" size="l" alignItems="center">
						<FormField
							label="AWS account"
							errorText={
								accountDraft && !/^\d{12}$/.test(accountDraft)
									? "Enter a 12-digit account ID."
									: undefined
							}
						>
							<Autosuggest
								value={accountDraft}
								options={accounts.map((value) => ({ value }))}
								ariaLabel="AWS account"
								placeholder="12-digit account ID"
								onChange={({ detail }) => setAccountDraft(detail.value)}
								onSelect={({ detail }) =>
									detail.value && go(search.section, "", false, detail.value)
								}
								enteredTextLabel={(value) => `Use account ${value}`}
							/>
						</FormField>
						<Button
							disabled={
								!/^\d{12}$/.test(accountDraft) || accountDraft === account
							}
							onClick={() => go(search.section, "", false, accountDraft)}
						>
							Switch account
						</Button>
						<Badge color="grey">Global service</Badge>
					</SpaceBetween>
					{notice && (
						<Flashbar
							items={[
								{
									id: "iam-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error" header="Cannot load IAM">
							{inventory.error || capabilities.error}
						</Alert>
					)}
					{capabilities.data?.requires_signature && (
						<Alert type="info" header="IAM management is read-only">
							This server requires signed requests. Use the AWS CLI or SDK to
							manage IAM resources. Console forms are available in parse mode.
						</Alert>
					)}
					{search.create && search.section !== "overview" ? (
						enabled ? (
							<IAMCreate
								key={`${account}/${search.section}`}
								kind={search.section}
								resources={resources}
								account={account}
								onCancel={() => go(search.section)}
								onCreated={(name) => {
									refresh();
									setNotice(
										`${iamLabels[search.section as IAMKind].singular} ${name} created.`,
									);
									go(search.section, name);
								}}
							/>
						) : (
							<Alert type="info">
								IAM creation is unavailable for this server.
							</Alert>
						)
					) : search.name ? (
						selected ? (
							<IAMDetail
								key={`${account}/${selected.type}/${selected.name}`}
								resource={selected}
								resources={resources}
								enabled={enabled}
								onRefresh={refresh}
								onNavigate={(kind, name) => go(kind, name)}
								onDeleted={() => {
									setNotice(
										`${iamLabels[search.section as IAMKind].singular} ${search.name} deleted.`,
									);
									refresh();
									go(search.section);
								}}
							/>
						) : (
							<Container>
								<EmptyState
									title={
										inventory.loading
											? "Loading IAM resource"
											: "Resource not found"
									}
									subtitle={`${search.name} was not found in account ${account}.`}
									action={
										<Button onClick={() => go(search.section)}>
											Back to {title.toLowerCase()}
										</Button>
									}
								/>
							</Container>
						)
					) : search.section === "overview" ? (
						<IAMOverview resources={resources} account={account} />
					) : (
						<IAMList
							key={`${account}/${search.section}`}
							kind={search.section}
							resources={resources.filter((r) => r.type === search.section)}
							loading={inventory.loading}
							enabled={enabled}
							onRefresh={refresh}
							onCreate={() => go(search.section, "", true)}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}

function IAMOverview({
	resources,
	account,
}: {
	resources: Resource[];
	account: string;
}) {
	const follow = useFollow();
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description={`Manage identities and access for account ${account}.`}
				>
					IAM dashboard
				</Header>
			}
		>
			<SpaceBetween size="l">
				<Container header={<Header variant="h2">IAM resources</Header>}>
					<ColumnLayout columns={4} variant="text-grid">
						{iamKinds.map((kind) => (
							<div key={kind}>
								<Box variant="h3">
									<Link href={iamHref(kind, account)} onFollow={follow}>
										{iamLabels[kind].plural}
									</Link>
								</Box>
								<Box variant="h1">
									{resources.filter((r) => r.type === kind).length}
								</Box>
							</div>
						))}
					</ColumnLayout>
				</Container>
				<Container header={<Header variant="h2">Account details</Header>}>
					<KeyValuePairs
						columns={2}
						items={[
							{ label: "Account ID", value: account },
							{ label: "Region", value: "Global" },
						]}
					/>
				</Container>
				<Alert type="info">
					TinyStack stores IAM permission policies and trust relationships.
					Permission policies are not evaluated when accessing services.
				</Alert>
			</SpaceBetween>
		</ContentLayout>
	);
}

function IAMList({
	kind,
	resources,
	loading,
	enabled,
	onRefresh,
	onCreate,
}: {
	kind: IAMKind;
	resources: Resource[];
	loading: boolean;
	enabled: boolean;
	onRefresh: () => void;
	onCreate: () => void;
}) {
	const follow = useFollow();
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(resources, {
			filtering: {
				empty: (
					<EmptyState
						title={`No ${iamLabels[kind].plural.toLowerCase()}`}
						subtitle={`Create a ${iamLabels[kind].singular.toLowerCase()} to get started.`}
					/>
				),
				noMatch: (
					<EmptyState
						title="No matches"
						subtitle="Change your search to find IAM resources."
					/>
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
			items={items}
			trackBy="arn"
			loading={loading}
			loadingText="Loading IAM resources"
			header={
				<Header
					variant="awsui-h1-sticky"
					counter={`(${resources.length})`}
					description={`Manage ${iamLabels[kind].plural.toLowerCase()} in this account.`}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh IAM resources"
								onClick={onRefresh}
							/>
							<Button variant="primary" disabled={!enabled} onClick={onCreate}>
								Create {iamLabels[kind].singular.toLowerCase()}
							</Button>
						</SpaceBetween>
					}
				>
					{iamLabels[kind].plural}
				</Header>
			}
			columnDefinitions={[
				{
					id: "name",
					header: `${iamLabels[kind].singular} name`,
					sortingField: "name",
					isRowHeader: true,
					cell: (r) => (
						<Link href={iamHref(kind, r.account, r.name)} onFollow={follow}>
							{r.name}
						</Link>
					),
				},
				{
					id: "path",
					header: "Path",
					cell: (r) => String(r.details?.Path ?? "/"),
				},
				...(kind === "group"
					? [
							{
								id: "users",
								header: "Users",
								cell: (r: Resource) =>
									(r.details?.Users as string[] | undefined)?.length ?? 0,
							},
						]
					: []),
				...(kind === "policy"
					? [
							{
								id: "attachments",
								header: "Attached entities",
								cell: (r: Resource) => String(r.details?.AttachmentCount ?? 0),
							},
						]
					: []),
				{ id: "arn", header: "ARN", cell: (r) => r.arn },
			]}
			filter={
				<TextFilter
					{...filterProps}
					filteringPlaceholder={`Find ${iamLabels[kind].plural.toLowerCase()}`}
					filteringAriaLabel="Find IAM resources"
				/>
			}
			pagination={<Pagination {...paginationProps} />}
		/>
	);
}
