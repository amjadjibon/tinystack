import {
	Alert,
	CopyToClipboard,
	Link,
	SpaceBetween,
	Tabs,
} from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleLayout } from "#/components/Console";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { ClusterDialog } from "#/components/eks/ClusterDialog";
import { type ResourceKind, Resources } from "#/components/eks/Resources";
import { clusters, eksCall, eksHref } from "#/lib/eks";
import {
	type Cluster,
	type EKSScope,
	kubeconfig,
	pending,
} from "#/lib/eksHelpers";
import { unixTime } from "#/lib/format";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { tagList, tagMap } from "#/lib/tags";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

const tabs = ["overview", "nodegroups", "addons", "fargate", "access", "tags"];
export const Route = createFileRoute("/services/eks")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		cluster: String(s.cluster ?? ""),
		tab: tabs.includes(String(s.tab)) ? String(s.tab) : "overview",
	}),
	component: EKSPage,
});
function EKSPage() {
	const { account, region, canWrite, capabilities, inventory } = useScope();
	const search = Route.useSearch();
	const enabled =
		!!capabilities.data?.pages.eks &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const scope = { account, region };
	const home = eksHref(scope);
	const docker = capabilities.data?.eks_runtime === "k3s";
	return (
		<ConsoleLayout
			navigationHeader={{ text: "Elastic Kubernetes Service", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Clusters", href: home },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "Elastic Kubernetes Service", href: home },
				...(search.cluster
					? [{ text: search.cluster, href: eksHref(scope, search.cluster) }]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.eks && (
						<Alert type="warning">EKS is not enabled on this server.</Alert>
					)}
					{!canWrite && (
						<Alert>
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<Alert
						header={
							docker ? "Docker runtime configured" : "Records-only runtime"
						}
					>
						{docker
							? "Each cluster runs a k3s Kubernetes API server. The page refreshes while resources change. Use the endpoint and certificate returned below once the cluster is ACTIVE."
							: "Clusters and related resources are stored as records. No Kubernetes API server runs, so endpoints are placeholders. Set eks.clusters to k3s for a working API server."}
					</Alert>
					<Workspace
						key={`${account}/${region}/${enabled}/${search.cluster}`}
						scope={scope}
						enabled={enabled}
						name={search.cluster}
						tab={search.tab}
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
	name,
	tab,
	onChanged,
}: {
	scope: EKSScope;
	enabled: boolean;
	name: string;
	tab: string;
	onChanged: () => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow();
	const [changing, setChanging] = useState(true);
	const poll = usePoll(
		async () => {
			const rows = enabled ? await clusters(scope) : [];
			setChanging(rows.some((c) => pending(c.Status)));
			return rows;
		},
		changing ? 2000 : 0,
	);
	const [dialog, setDialog] = useState<
		"" | "create" | "config" | "version" | "delete"
	>("");
	const c = poll.data?.find((c) => c.Name === name);
	const saved = () => {
		setDialog("");
		setChanging(true);
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!name ? (
				<Rows
					title="Clusters"
					items={poll.data ?? []}
					loading={poll.loading}
					emptyText="Choose Create cluster to add one."
					actions={
						<Buttons>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action disabled={!enabled} onClick={() => setDialog("create")}>
								Create cluster
							</Action>
						</Buttons>
					}
					columns={[
						{
							id: "name",
							header: "Name",
							cell: (c) => (
								<Link href={eksHref(scope, c.Name)} onFollow={follow}>
									{c.Name}
								</Link>
							),
						},
						{ id: "version", header: "Version", cell: (c) => c.Version ?? "-" },
						{ id: "status", header: "Status", cell: (c) => c.Status ?? "-" },
						{
							id: "endpoint",
							header: "Endpoint",
							cell: (c) => c.Endpoint ?? "-",
						},
						{
							id: "created",
							header: "Created",
							cell: (c) => unixTime(c.CreatedAt),
						},
					]}
				/>
			) : c ? (
				<>
					<Buttons>
						<Action onClick={() => navigate({ href: eksHref(scope) })}>
							Clusters
						</Action>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled || pending(c.Status)}
							onClick={() => setDialog("config")}
						>
							Edit configuration
						</Action>
						<Action
							disabled={!enabled || pending(c.Status)}
							onClick={() => setDialog("version")}
						>
							Update version
						</Action>
						<Action
							disabled={!enabled || pending(c.Status)}
							onClick={() => setDialog("delete")}
						>
							Delete cluster
						</Action>
					</Buttons>
					<Tabs
						activeTabId={tab}
						onChange={({ detail }) =>
							navigate({ href: eksHref(scope, name, detail.activeTabId) })
						}
						tabs={tabs.map((id) => ({
							id,
							label: {
								overview: "Overview",
								nodegroups: "Node groups",
								addons: "Add-ons",
								fargate: "Fargate profiles",
								access: "Access entries",
								tags: "Tags",
							}[id],
							content:
								tab !== id ? null : id === "overview" ? (
									<Overview cluster={c} region={scope.region} />
								) : id === "tags" ? (
									<ConsoleTags
										tags={tagMap(tagList(c.Tags ?? {}))}
										enabled={enabled}
										onChanged={saved}
										plan={(next, removed) => [
											...(Object.keys(next).length
												? [
														{
															label: "TagResource",
															run: () =>
																eksCall(scope, "TagResource", {
																	ResourceArn: c.Arn,
																	Tags: next,
																}),
														},
													]
												: []),
											...(removed.length
												? [
														{
															label: "UntagResource",
															run: () =>
																eksCall(scope, "UntagResource", {
																	ResourceArn: c.Arn,
																	TagKeys: removed,
																}),
														},
													]
												: []),
										]}
									/>
								) : (
									<Resources
										key={id}
										scope={scope}
										cluster={c}
										kind={id as ResourceKind}
										enabled={enabled}
										onChanged={onChanged}
									/>
								),
						}))}
					/>
				</>
			) : (
				!poll.loading && (
					<Alert>Cluster not found in this account and Region.</Alert>
				)
			)}
			{(dialog === "create" ||
				((dialog === "config" || dialog === "version") && !!c)) && (
				<ClusterDialog
					scope={scope}
					cluster={dialog === "create" ? undefined : c}
					versionOnly={dialog === "version"}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && !!c && (
				<ActionDialog
					title="Delete cluster"
					confirmation={c.Name}
					onClose={() => setDialog("")}
					onSaved={() => {
						saved();
						navigate({ href: eksHref(scope) });
					}}
					plan={async () => [
						{
							label: "DeleteCluster",
							run: () => eksCall(scope, "DeleteCluster", { Name: c.Name }),
						},
					]}
				>
					<Alert type="warning">
						Delete {c.Name} permanently. Delete its node groups and Fargate
						profiles first.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function Overview({
	cluster: c,
	region,
}: {
	cluster: Cluster;
	region: string;
}) {
	const config = kubeconfig(c, region);
	const copy = (text: string, label: string) => (
		<CopyToClipboard
			textToCopy={text}
			copySuccessText={`${label} copied`}
			copyErrorText="Copy failed"
			variant="inline"
		/>
	);
	return (
		<SpaceBetween size="m">
			<div>
				{c.Name} · Kubernetes {c.Version} · {c.Status} · Created{" "}
				{unixTime(c.CreatedAt)}
			</div>
			<div>Endpoint: {c.Endpoint ? copy(c.Endpoint, "Endpoint") : "-"}</div>
			<div>
				Certificate authority (base64 PEM):{" "}
				{c.CertificateAuthority?.Data
					? copy(c.CertificateAuthority.Data, "Certificate")
					: "-"}
			</div>
			<div>
				OIDC issuer:{" "}
				{c.Identity?.Oidc?.Issuer
					? copy(c.Identity.Oidc.Issuer, "Issuer")
					: "-"}
			</div>
			<div>Kubeconfig: {copy(config, "Kubeconfig")}</div>
			<pre style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
				{config}
			</pre>
			<Alert>
				Save this as a kubeconfig file and set KUBECONFIG to its path. The exec
				credential plugin requires the AWS CLI with credentials for this
				account. Records mode has no API server.
			</Alert>
		</SpaceBetween>
	);
}
