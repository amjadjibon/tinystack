import { useCollection } from "@cloudscape-design/collection-hooks";
import {
	Alert,
	Box,
	Button,
	Checkbox,
	Container,
	ContentLayout,
	Flashbar,
	FormField,
	Header,
	Input,
	Link,
	Pagination,
	SpaceBetween,
	Table,
	TextFilter,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect, useMemo, useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { S3Bucket } from "#/components/s3/S3Bucket";
import { S3Dialog } from "#/components/s3/S3Dialog";
import type { Resource } from "#/lib/api";
import {
	emptyBucketSteps,
	type S3Scope,
	type S3Step,
	s3Call,
	s3Href,
	sizeLabel,
	validRegion,
} from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";

export const Route = createFileRoute("/services/s3")({
	validateSearch: (search: Record<string, unknown>) => ({
		account: String(search.account ?? ""),
		region: String(search.region ?? ""),
		bucket: String(search.bucket ?? ""),
		prefix: String(search.prefix ?? ""),
		key: String(search.key ?? ""),
		version: String(search.version ?? ""),
	}),
	component: S3Page,
});
function S3Page() {
	const search = Route.useSearch();
	const navigate = useNavigate();
	const { canWrite, account, region, inventory, capabilities } = useScope();
	const scope = useMemo(
		() => ({ account, region, bucket: search.bucket }),
		[account, region, search.bucket],
	);
	const [notice, setNotice] = useState("");
	// biome-ignore lint/correctness/useExhaustiveDependencies: clear the previous scope’s success notice when switching.
	useEffect(() => setNotice(""), [account, region]);
	const enabled =
		!!capabilities.data?.s3 &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const readable = !!capabilities.data?.s3 && canWrite;
	const go = (next: S3Scope, prefix = "", key = "", version = "") =>
		navigate({ to: "/services/s3", search: { ...next, prefix, key, version } });
	const refresh = (message: string) => {
		inventory.reload();
		setNotice(message);
	};
	return (
		<ConsoleLayout
			contentType={search.bucket ? "default" : "table"}
			navigationHeader={{
				text: "Amazon S3",
				href: s3Href({ account, region }),
			}}
			activeHref={s3Href({ account, region })}
			navigationItems={[
				{ type: "link", text: "Buckets", href: s3Href({ account, region }) },
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "Amazon S3", href: s3Href({ account, region }) },
				{ text: "Buckets", href: s3Href({ account, region }) },
				...(search.bucket
					? [{ text: search.bucket, href: s3Href(scope) }]
					: []),
				...search.prefix
					.split("/")
					.filter(Boolean)
					.map((name, i, parts) => ({
						text: name,
						href: s3Href(scope, `${parts.slice(0, i + 1).join("/")}/`),
					})),
				...(search.key
					? [{ text: search.key.slice(search.prefix.length), href: "#" }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{notice && (
						<Flashbar
							items={[
								{
									id: "s3-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error" header="Cannot load S3">
							{inventory.error || capabilities.error}
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info" header="S3 management is read-only">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{search.bucket ? (
						<S3Bucket
							key={JSON.stringify(scope)}
							scope={scope}
							prefix={search.prefix}
							objectKey={search.key}
							version={search.version}
							enabled={enabled}
							readable={readable}
							onNavigate={(prefix, key = "", version = "") =>
								go(scope, prefix, key, version)
							}
							onChanged={refresh}
							onDeleted={() => {
								refresh(`Bucket ${scope.bucket} deleted.`);
								go({ ...scope, bucket: "" });
							}}
						/>
					) : (
						<BucketList
							key={`${account}/${region}`}
							scope={scope}
							resources={(inventory.data ?? []).filter(
								(r) =>
									r.service === "s3" &&
									r.type === "bucket" &&
									r.account === account &&
									r.region === region,
							)}
							loading={inventory.loading}
							enabled={enabled}
							onOpen={(bucket) => go({ ...scope, bucket })}
							onChanged={refresh}
							onRefresh={inventory.reload}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}

function BucketList({
	scope,
	resources,
	loading,
	enabled,
	onOpen,
	onChanged,
	onRefresh,
}: {
	scope: S3Scope;
	resources: Resource[];
	loading: boolean;
	enabled: boolean;
	onOpen: (bucket: string) => void;
	onChanged: (message: string) => void;
	onRefresh: () => void;
}) {
	const follow = useFollow();
	const collection = useCollection(resources, {
		filtering: {},
		pagination: { pageSize: 20 },
		sorting: {},
		selection: { trackBy: "arn" },
	});
	const selected = (collection.collectionProps.selectedItems ?? [])[0];
	const [dialog, setDialog] = useState<"create" | "empty" | "delete" | null>(
		null,
	);
	const [targetBucket, setTargetBucket] = useState("");
	const [name, setName] = useState("");
	const [versioning, setVersioning] = useState(false);
	const target = { ...scope, bucket: targetBucket };
	const plan = async (): Promise<S3Step[]> => {
		if (dialog === "empty") return emptyBucketSteps(target);
		if (dialog === "delete")
			return [
				{ label: "Delete bucket", run: () => s3Call(target, "DeleteBucket") },
			];
		if (!/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(name))
			throw new Error(
				"Use a bucket name with 3–63 lowercase letters, numbers, dots or hyphens.",
			);
		if (resources.some((r) => r.name === name))
			throw new Error("This bucket already exists in the selected scope.");
		const next = { ...scope, bucket: name };
		return [
			{
				label: "Create bucket",
				run: () =>
					s3Call(
						next,
						"CreateBucket",
						scope.region === "us-east-1"
							? {}
							: {
									CreateBucketConfiguration: {
										LocationConstraint: scope.region,
									},
								},
					),
			},
			...(versioning
				? [
						{
							label: "Enable versioning",
							run: () =>
								s3Call(next, "PutBucketVersioning", {
									VersioningConfiguration: { Status: "Enabled" },
								}),
						},
					]
				: []),
		];
	};
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description="General purpose buckets in the selected account and Region."
				>
					Buckets
				</Header>
			}
		>
			<Table
				{...collection.collectionProps}
				items={collection.items}
				loading={loading}
				loadingText="Loading buckets"
				trackBy="arn"
				selectionType="single"
				ariaLabels={{
					selectionGroupLabel: "Select bucket",
					itemSelectionLabel: (_, r) => `Select ${r.name}`,
				}}
				selectedItems={collection.collectionProps.selectedItems ?? []}
				onSelectionChange={collection.collectionProps.onSelectionChange}
				columnDefinitions={[
					{
						id: "name",
						header: "Bucket name",
						sortingField: "name",
						cell: (r) => (
							<Link
								href={s3Href({ ...scope, bucket: r.name })}
								onFollow={follow}
							>
								{r.name}
							</Link>
						),
					},
					{ id: "region", header: "AWS Region", cell: (r) => r.region },
					{
						id: "objects",
						header: "Objects",
						cell: (r) => String(r.details?.Objects ?? 0),
					},
					{
						id: "size",
						header: "Total size",
						cell: (r) => sizeLabel(Number(r.details?.Size ?? 0)),
					},
					{
						id: "created",
						header: "Creation date",
						cell: (r) => String(r.details?.CreationDate ?? "—"),
					},
				]}
				header={
					<Header
						counter={`(${resources.length})`}
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									iconName="refresh"
									ariaLabel="Refresh buckets"
									onClick={onRefresh}
								/>
								<Button
									disabled={!selected || !enabled}
									onClick={() => {
										setTargetBucket(selected?.name ?? "");
										setDialog("empty");
									}}
								>
									Empty
								</Button>
								<Button
									disabled={!selected || !enabled}
									onClick={() => {
										setTargetBucket(selected?.name ?? "");
										setDialog("delete");
									}}
								>
									Delete
								</Button>
								<Button
									variant="primary"
									disabled={!enabled}
									onClick={() => {
										setName("");
										setVersioning(false);
										setDialog("create");
									}}
								>
									Create bucket
								</Button>
							</SpaceBetween>
						}
					>
						General purpose buckets
					</Header>
				}
				filter={
					<TextFilter
						{...collection.filterProps}
						filteringPlaceholder="Find buckets"
					/>
				}
				pagination={<Pagination {...collection.paginationProps} />}
				empty={
					<Box textAlign="center" padding="l">
						<SpaceBetween size="m">
							<Box variant="h3">No buckets</Box>
							<Box>Create a bucket to start storing objects.</Box>
						</SpaceBetween>
					</Box>
				}
			/>
			{dialog && (
				<S3Dialog
					title={
						dialog === "create"
							? "Create bucket"
							: `${dialog === "empty" ? "Empty" : "Delete"} bucket ${target.bucket}`
					}
					submit={
						dialog === "create"
							? "Create bucket"
							: dialog === "empty"
								? "Empty bucket"
								: "Delete bucket"
					}
					confirmation={dialog === "create" ? undefined : target.bucket}
					plan={plan}
					onClose={() => setDialog(null)}
					onSaved={() => {
						const mode = dialog;
						setDialog(null);
						onChanged(
							mode === "create"
								? `Bucket ${name} created.`
								: mode === "empty"
									? `Bucket ${target.bucket} emptied.`
									: `Bucket ${target.bucket} deleted.`,
						);
						if (mode === "create") onOpen(name);
					}}
				>
					{dialog === "create" ? (
						<SpaceBetween size="l">
							<Container
								header={<Header variant="h2">General configuration</Header>}
							>
								<SpaceBetween size="l">
									<FormField label="AWS Region">
										<Box>{scope.region}</Box>
									</FormField>
									<FormField
										label="Bucket name"
										description="Bucket names must be unique across all accounts and Regions."
									>
										<Input
											ariaLabel="Bucket name"
											value={name}
											onChange={({ detail }) => setName(detail.value)}
										/>
									</FormField>
								</SpaceBetween>
							</Container>
							<Container
								header={
									<Header variant="h2">
										Object ownership and public access
									</Header>
								}
							>
								<Box>
									ACLs are disabled (Bucket owner enforced). All Block Public
									Access settings are enabled.
								</Box>
							</Container>
							<Checkbox
								checked={versioning}
								onChange={({ detail }) => setVersioning(detail.checked)}
							>
								Enable bucket versioning
							</Checkbox>
						</SpaceBetween>
					) : (
						<Alert type="warning">
							{dialog === "empty"
								? "Permanently delete all objects, every version, delete markers and unfinished multipart uploads. These changes cannot be undone."
								: "The bucket must be empty, including versions and unfinished uploads. Empty it first if deletion reports a conflict."}
						</Alert>
					)}
				</S3Dialog>
			)}
		</ContentLayout>
	);
}
