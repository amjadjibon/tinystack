import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	CopyToClipboard,
	Header,
	Link,
	Pagination,
	SpaceBetween,
	StatusIndicator,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useState } from "react";
import { CertificateDetails } from "#/components/acm/CertificateDetails";
import { Choice } from "#/components/acm/Fields";
import { ImportDialog } from "#/components/acm/ImportDialog";
import { RequestDialog } from "#/components/acm/RequestDialog";
import { ConsoleLayout } from "#/components/Console";
import {
	type ACMScope,
	acmHref,
	type CertificateSummary,
	listCertificates,
} from "#/lib/acm";
import {
	certificateStatuses,
	certificateType,
	formatEpoch,
} from "#/lib/acmHelpers";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/services/acm")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		certificate: String(s.certificate ?? ""),
	}),
	component: ACMPage,
});

function ACMPage() {
	const search = Route.useSearch(),
		{ account, region, canWrite, capabilities, inventory } = useScope();
	const scope = { account, region, certificate: search.certificate },
		list = acmHref({ account, region });
	const enabled =
		!!capabilities.data?.acm &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	return (
		<ConsoleLayout
			contentType={search.certificate ? "default" : "table"}
			navigationHeader={{ text: "Certificate Manager", href: list }}
			activeHref={list}
			navigationItems={[
				{ type: "link", text: "Certificates", href: list },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "Certificates", href: list },
				...(search.certificate
					? [{ text: search.certificate, href: acmHref(scope) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{capabilities.error && (
						<Alert type="error">{capabilities.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.acm && (
						<Alert type="warning">
							Certificate Manager is not enabled on this server.
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root console session for the selected account to manage
							certificates.
						</Alert>
					)}
					<Workspace
						key={`${account}/${region}/${search.certificate}/${enabled}`}
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
	scope: ACMScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const navigate = useNavigate();
	const [dialog, setDialog] = useState<"" | "request" | "import">(""),
		[revision, setRevision] = useState(0);
	const base = { account: scope.account, region: scope.region };
	const created = (arn: string) => {
		setDialog("");
		onChanged();
		setRevision((r) => r + 1);
		if (arn) navigate({ href: acmHref({ ...base, certificate: arn }) });
	};
	return (
		<>
			{scope.certificate ? (
				<CertificateDetails
					scope={scope}
					enabled={enabled}
					onChanged={onChanged}
					onDeleted={() => {
						onChanged();
						navigate({ href: acmHref(base) });
					}}
				/>
			) : (
				<CertificateList
					key={revision}
					scope={scope}
					enabled={enabled}
					onRequest={() => setDialog("request")}
					onImport={() => setDialog("import")}
				/>
			)}
			{dialog === "request" && (
				<RequestDialog
					scope={scope}
					onClose={() => setDialog("")}
					onSaved={created}
				/>
			)}
			{dialog === "import" && (
				<ImportDialog
					scope={scope}
					onClose={() => setDialog("")}
					onSaved={created}
				/>
			)}
		</>
	);
}

function CertificateList({
	scope,
	enabled,
	onRequest,
	onImport,
}: {
	scope: ACMScope;
	enabled: boolean;
	onRequest: () => void;
	onImport: () => void;
}) {
	const follow = useFollow();
	const [status, setStatus] = useState("");
	const poll = usePoll(() =>
		enabled
			? listCertificates(scope, status ? [status] : [])
			: Promise.resolve([]),
	);
	const reload = poll.reload;
	// Reload at once when the status filter changes instead of waiting for the next poll.
	// biome-ignore lint/correctness/useExhaustiveDependencies: status is the trigger
	useEffect(() => reload(), [status, reload]);
	const certificates = poll.data ?? [];
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(certificates, {
			filtering: {
				filteringFunction: (item, text) =>
					[item.DomainName, item.CertificateArn, item.Status, item.Type]
						.join(" ")
						.toLowerCase()
						.includes(text.toLowerCase()),
				empty: (
					<Box>
						No certificates. Choose Request certificate or Import certificate to
						add one.
					</Box>
				),
				noMatch: (
					<Box>No matches. Clear the filter to see all certificates.</Box>
				),
			},
			sorting: {
				defaultState: { sortingColumn: { sortingField: "DomainName" } },
			},
			pagination: { pageSize: 20 },
		});
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Table
				{...collectionProps}
				items={items}
				trackBy="CertificateArn"
				loading={poll.loading}
				variant="full-page"
				filter={
					<SpaceBetween direction="horizontal" size="s">
						<TextFilter
							{...filterProps}
							filteringPlaceholder="Find certificates"
						/>
						<Choice
							label="Status filter"
							value={status}
							options={["", ...certificateStatuses]}
							onChange={setStatus}
							description="Leave empty to show every status."
						/>
					</SpaceBetween>
				}
				pagination={<Pagination {...paginationProps} />}
				header={
					<Header
						variant="h1"
						counter={`(${certificates.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button onClick={poll.reload}>Refresh</Button>
								<Button disabled={!enabled} onClick={onImport}>
									Import certificate
								</Button>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={onRequest}
								>
									Request certificate
								</Button>
							</SpaceBetween>
						}
					>
						Certificates
					</Header>
				}
				columnDefinitions={[
					{
						id: "domain",
						header: "Domain name",
						sortingField: "DomainName",
						cell: (c: CertificateSummary) => (
							<Link
								href={acmHref({ ...scope, certificate: c.CertificateArn })}
								onFollow={follow}
							>
								{c.DomainName}
							</Link>
						),
					},
					{
						id: "names",
						header: "Additional names",
						cell: (c) =>
							Math.max(
								(c.SubjectAlternativeNameSummaries?.length ?? 1) - 1,
								0,
							) + (c.HasAdditionalSubjectAlternativeNames ? "+" : ""),
					},
					{
						id: "status",
						header: "Status",
						sortingField: "Status",
						cell: (c) => (
							<StatusIndicator
								type={
									c.Status === "ISSUED"
										? "success"
										: c.Status === "PENDING_VALIDATION"
											? "pending"
											: "error"
								}
							>
								{c.Status}
							</StatusIndicator>
						),
					},
					{
						id: "type",
						header: "Type",
						sortingField: "Type",
						cell: (c) => certificateType(c.Type),
					},
					{
						id: "inuse",
						header: "In use",
						cell: (c) => (c.InUse ? "Yes" : "No"),
					},
					{
						id: "algorithm",
						header: "Key algorithm",
						sortingField: "KeyAlgorithm",
						cell: (c) => c.KeyAlgorithm ?? "—",
					},
					{
						id: "notAfter",
						header: "Not after",
						sortingField: "NotAfter",
						cell: (c) => formatEpoch(c.NotAfter),
					},
					{
						id: "arn",
						header: "ARN",
						cell: (c) => (
							<CopyToClipboard
								variant="inline"
								textToCopy={c.CertificateArn}
								copySuccessText="ARN copied"
								copyErrorText="ARN failed to copy"
							/>
						),
					},
				]}
			/>
		</SpaceBetween>
	);
}
