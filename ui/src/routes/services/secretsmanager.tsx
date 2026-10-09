import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Checkbox,
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
import { SecretDetails } from "#/components/secretsmanager/SecretDetails";
import { SecretForm } from "#/components/secretsmanager/SecretForm";
import type { Resource } from "#/lib/api";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import {
	type Secret,
	type SecretScope,
	secretDate,
	secretHref,
	secretPages,
} from "#/lib/secretsmanager";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/secretsmanager")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		secret: String(search.secret ?? ""),
	}),
	component: SecretsManagerPage,
});
function SecretsManagerPage() {
	const search = Route.useSearch(),
		{ account, region, inventory, capabilities } = useScope();
	const enabled =
		!!capabilities.data?.secretsmanager &&
		!capabilities.data.requires_signature &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region, secret: search.secret };
	const list = secretHref({ account, region });
	return (
		<ConsoleLayout
			contentType={search.secret ? "default" : "table"}
			navigationHeader={{ text: "Secrets Manager", href: list }}
			activeHref={list}
			navigationItems={[
				{ type: "link", text: "Secrets", href: list },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
				{ type: "link", text: "Delivery log", href: "/deliveries" },
			]}
			breadcrumbs={[
				{ text: "Secrets Manager", href: list },
				{ text: "Secrets", href: list },
				...(search.secret
					? [
							{
								text: search.secret.split(":secret:").at(-1) ?? search.secret,
								href: secretHref(scope),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data?.requires_signature && (
						<Alert type="info" header="Secrets Manager management is read-only">
							Secret metadata inventory is available here. Use signed AWS CLI or
							SDK requests for values, versions, rotation, policies, and
							management.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.secretsmanager && (
						<Alert type="warning">
							Secrets Manager is not enabled on this server.
						</Alert>
					)}
					<SecretWorkspace
						key={`${account}/${region}/${search.secret}/${enabled}`}
						scope={scope}
						enabled={enabled}
						resources={(inventory.data ?? []).filter(
							(r) =>
								r.service === "secretsmanager" &&
								r.type === "secret" &&
								r.account === account &&
								r.region === region,
						)}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}
function SecretWorkspace({
	scope,
	enabled,
	resources,
	onChanged,
}: {
	scope: SecretScope;
	enabled: boolean;
	resources: Resource[];
	onChanged: () => void;
}) {
	const navigate = useNavigate(),
		[create, setCreate] = useState(false),
		[include, setInclude] = useState(false),
		[revision, setRevision] = useState(0);
	const alive = useRef(true);
	useEffect(() => {
		alive.current = true;
		return () => {
			alive.current = false;
		};
	}, []);
	const changed = () => {
		if (alive.current) {
			onChanged();
			setRevision((r) => r + 1);
		}
	};
	return (
		<>
			{scope.secret ? (
				<SecretDetails
					scope={scope}
					enabled={enabled}
					resource={resources.find(
						(r) => r.arn === scope.secret || r.name === scope.secret,
					)}
					onChanged={changed}
					onDeleted={() => {
						if (alive.current)
							navigate({
								href: secretHref({
									account: scope.account,
									region: scope.region,
								}),
							});
					}}
				/>
			) : (
				<SecretList
					key={`${include}/${revision}`}
					{...{ scope, enabled, resources, include }}
					onInclude={setInclude}
					onCreate={() => setCreate(true)}
				/>
			)}
			{create && enabled && (
				<SecretForm
					scope={scope}
					create
					onClose={() => setCreate(false)}
					onSaved={(arn) => {
						if (!alive.current) return;
						setCreate(false);
						changed();
						navigate({ href: secretHref({ ...scope, secret: arn }) });
					}}
				/>
			)}
		</>
	);
}
function SecretList({
	scope,
	enabled,
	resources,
	include,
	onInclude,
	onCreate,
}: {
	scope: SecretScope;
	enabled: boolean;
	resources: Resource[];
	include: boolean;
	onInclude: (v: boolean) => void;
	onCreate: () => void;
}) {
	const poll = usePoll(() =>
		enabled
			? secretPages<Secret>(scope, "ListSecrets", "SecretList", {
					IncludePlannedDeletion: include,
					SortOrder: "desc",
				})
			: Promise.resolve(undefined),
	);
	const follow = useFollow();
	const secrets: Secret[] =
		poll.data ??
		resources.map((r) => ({
			ARN: r.arn,
			Name: r.name,
			Description: String(r.details?.Description ?? ""),
			DeletedDate: r.details?.DeletionDate as number | undefined,
		}));
	const shown = secrets.filter((s) => include || !s.DeletedDate);
	const { items, collectionProps, filterProps, paginationProps } =
		useCollection(shown, {
			filtering: {
				empty: <Box>No secrets in this account and Region.</Box>,
				noMatch: <Box>No matching secrets.</Box>,
			},
			sorting: { defaultState: { sortingColumn: { sortingField: "Name" } } },
			pagination: { pageSize: 20 },
		});
	return (
		<SpaceBetween size="m">
			{poll.error && (
				<Alert type="error" header="Cannot load secrets">
					{poll.error}
				</Alert>
			)}
			<Table
				{...collectionProps}
				items={items}
				trackBy="ARN"
				loading={poll.loading}
				loadingText="Loading secrets"
				variant="full-page"
				header={
					<Header
						variant="awsui-h1-sticky"
						counter={`(${shown.length})`}
						description="Manage secrets and their versions in the selected account and Region."
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh secrets"
									onClick={poll.reload}
								/>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={onCreate}
								>
									Store a new secret
								</Button>
							</SpaceBetween>
						}
					>
						Secrets
					</Header>
				}
				filter={
					<SpaceBetween size="s">
						<TextFilter {...filterProps} filteringPlaceholder="Find secrets" />
						<Checkbox
							checked={include}
							onChange={({ detail }) => onInclude(detail.checked)}
						>
							Show secrets scheduled for deletion
						</Checkbox>
					</SpaceBetween>
				}
				pagination={<Pagination {...paginationProps} />}
				columnDefinitions={[
					{
						id: "name",
						header: "Secret name",
						cell: (s) => (
							<Link
								href={secretHref({ ...scope, secret: s.ARN })}
								onFollow={follow}
							>
								{s.Name}
							</Link>
						),
						sortingField: "Name",
						isRowHeader: true,
					},
					{
						id: "description",
						header: "Description",
						cell: (s) => s.Description || "—",
					},
					{
						id: "status",
						header: "Status",
						cell: (s) => (s.DeletedDate ? "Scheduled for deletion" : "Active"),
					},
					{
						id: "changed",
						header: "Last changed",
						cell: (s) => secretDate(s.LastChangedDate),
						sortingField: "LastChangedDate",
					},
					{
						id: "rotation",
						header: "Automatic rotation",
						cell: (s) =>
							enabled ? (s.RotationEnabled ? "Enabled" : "Disabled") : "—",
					},
				]}
			/>
		</SpaceBetween>
	);
}
