import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	Flashbar,
	Header,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleLayout } from "#/components/Console";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	apiCall,
	type GatewayIntegration,
	type GatewayKind,
	type GatewayRoute,
	type GatewayScope,
	type GatewaySnapshot,
	gatewayARN,
	gatewayHref,
	gatewaySnapshot,
	gatewayTagPlan,
	isHTTP,
} from "#/lib/apigateway";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { ApiForm } from "./ApiForm";
import { Authorizers } from "./Authorizers";
import { GatewayTable } from "./GatewayTable";
import { HttpRouteForm, IntegrationForm } from "./HttpForms";
import { RequestTest } from "./RequestTest";
import { RestResources } from "./RestResources";
import { Stages } from "./Stages";

const sections: Record<string, string> = {
	routes: "Routes and resources",
	integrations: "Integrations",
	authorizers: "Authorizers",
	stages: "Stages",
	settings: "API settings",
	test: "Test requests",
};
export function gatewaySearch(s: Record<string, unknown>) {
	return {
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		apiId: String(s.apiId ?? ""),
		resourceId: String(s.resourceId ?? ""),
		method: String(s.method ?? ""),
		section:
			typeof s.section === "string" && Object.hasOwn(sections, s.section)
				? s.section
				: "apis",
	};
}
export function GatewayConsole({
	kind,
	search,
}: {
	kind: GatewayKind;
	search: ReturnType<typeof gatewaySearch>;
}) {
	const context = useScope(),
		[notice, setNotice] = useState({ scope: "", message: "" });
	const scopeId = `${context.account}/${context.region}/${kind}`;
	const enabled =
		!!context.capabilities.data?.[kind] &&
		context.canWrite &&
		/^\d{12}$/.test(context.account) &&
		validRegion(context.region);
	return (
		<Workspace
			key={`${scopeId}/${search.apiId}/${search.section}/${search.resourceId}/${search.method}/${enabled}`}
			scope={{
				account: context.account,
				region: context.region,
				kind,
				apiId: search.apiId,
				resourceId: search.resourceId,
				method: search.method,
			}}
			section={search.section}
			enabled={enabled}
			notice={notice.scope === scopeId ? notice.message : ""}
			setNotice={(message) => setNotice({ scope: scopeId, message })}
		/>
	);
}
function Workspace({
	scope,
	section,
	enabled,
	notice,
	setNotice,
}: {
	scope: GatewayScope;
	section: string;
	enabled: boolean;
	notice: string;
	setNotice: (m: string) => void;
}) {
	const { canWrite, inventory, capabilities } = useScope(),
		follow = useFollow(),
		navigate = useNavigate(),
		mounted = useRef(true),
		[creating, setCreating] = useState(false);
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	const http = isHTTP(scope),
		title = http ? "HTTP APIs" : "REST APIs",
		listScope = { ...scope, apiId: "", resourceId: "", method: "" },
		listHref = gatewayHref(listScope);
	const go = (next: GatewayScope, nextSection = "routes") => {
		if (mounted.current) navigate({ href: gatewayHref(next, nextSection) });
	};
	const changed = (message: string) => {
		inventory.reload();
		if (mounted.current) setNotice(message);
	};
	const rows = (inventory.data ?? []).filter(
		(r) =>
			r.service === scope.kind &&
			r.type === (http ? "http-api" : "rest-api") &&
			r.account === scope.account &&
			r.region === scope.region,
	);
	const selected = rows.find((r) => r.details?.ApiId === scope.apiId);
	const availableSections = Object.entries(sections).filter(
		([id]) => http || id !== "integrations",
	);
	return (
		<ConsoleLayout
			contentType={scope.apiId ? "default" : "table"}
			navigationHeader={{ text: "API Gateway", href: listHref }}
			activeHref={
				scope.apiId
					? gatewayHref({ ...scope, resourceId: "", method: "" }, section)
					: listHref
			}
			navigationItems={[
				{
					type: "link",
					text: "HTTP APIs",
					href: gatewayHref({ ...listScope, kind: "apigatewayv2" }),
				},
				{
					type: "link",
					text: "REST APIs",
					href: gatewayHref({ ...listScope, kind: "apigateway" }),
				},
				...(scope.apiId
					? [
							{ type: "divider" as const },
							...availableSections.map(([id, text]) => ({
								type: "link" as const,
								text:
									id === "routes"
										? http
											? "Routes"
											: "Resources and methods"
										: text,
								href: gatewayHref({ ...scope, resourceId: "", method: "" }, id),
							})),
						]
					: []),
				{ type: "divider" },
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{ text: "API Gateway", href: listHref },
				{ text: title, href: listHref },
				...(scope.apiId
					? [
							{
								text: selected?.name ?? scope.apiId,
								href: gatewayHref(scope, "settings"),
							},
							{
								text: sections[section] ?? "API settings",
								href: gatewayHref(scope, section),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{notice && (
						<Flashbar
							items={[
								{
									id: "gateway-success",
									type: "success",
									content: notice,
									dismissible: true,
									onDismiss: () => setNotice(""),
								},
							]}
						/>
					)}
					{(inventory.error || capabilities.error) && (
						<Alert type="error">{inventory.error || capabilities.error}</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data[scope.kind] && (
						<Alert type="warning">
							{title} are not enabled on this server.
						</Alert>
					)}
					{scope.apiId ? (
						<ApiWorkspace
							scope={scope}
							section={section}
							enabled={enabled}
							onChanged={changed}
							onDeleted={() => go(listScope, "apis")}
							onNavigate={(resourceId, method = "") =>
								go({ ...scope, resourceId, method })
							}
							readonlyDetails={selected?.details}
						/>
					) : (
						<GatewayTable
							items={rows}
							trackBy="arn"
							loading={inventory.loading}
							header={
								<Header
									variant="h1"
									counter={`(${rows.length})`}
									description="Create and manage APIs in the selected account and Region."
									actions={
										<SpaceBetween direction="horizontal" size="xs">
											<Button
												iconName="refresh"
												ariaLabel="Refresh APIs"
												onClick={inventory.reload}
											/>
											<Button
												variant="primary"
												disabled={!enabled}
												onClick={() => setCreating(true)}
											>
												Create API
											</Button>
										</SpaceBetween>
									}
								>
									{title}
								</Header>
							}
							columnDefinitions={[
								{
									id: "name",
									header: "Name",
									sortingField: "name",
									cell: (r) => (
										<Link
											href={gatewayHref(
												{ ...listScope, apiId: String(r.details?.ApiId ?? "") },
												"routes",
											)}
											onFollow={follow}
										>
											{r.name}
										</Link>
									),
								},
								{
									id: "id",
									header: "API ID",
									cell: (r) => String(r.details?.ApiId ?? "—"),
								},
								{
									id: "routes",
									header: http ? "Routes" : "Methods",
									cell: (r) =>
										Array.isArray(r.details?.[http ? "Routes" : "Methods"])
											? (
													(r.details?.[http ? "Routes" : "Methods"] ??
														[]) as unknown[]
												).length
											: 0,
								},
								{
									id: "stages",
									header: "Stages",
									cell: (r) =>
										Array.isArray(r.details?.Stages)
											? r.details.Stages.join(", ") || "None"
											: "None",
								},
							]}
						/>
					)}
					{creating && (
						<ApiForm
							scope={listScope}
							onClose={() => setCreating(false)}
							onSaved={(id) => {
								setCreating(false);
								changed("API created.");
								go({ ...listScope, apiId: id });
							}}
						/>
					)}
				</SpaceBetween>
			}
		/>
	);
}
function ApiWorkspace({
	scope,
	section,
	enabled,
	onChanged,
	onDeleted,
	onNavigate,
	readonlyDetails,
}: {
	scope: GatewayScope;
	section: string;
	enabled: boolean;
	onChanged: (m: string) => void;
	onDeleted: () => void;
	onNavigate: (resourceId: string, method?: string) => void;
	readonlyDetails?: Record<string, unknown>;
}) {
	const poll = usePoll(
			() => (enabled ? gatewaySnapshot(scope) : Promise.resolve(undefined)),
			0,
		),
		[dialog, setDialog] = useState("");
	const http = isHTTP(scope),
		snapshot = poll.data;
	const changed = (message: string) => {
		poll.reload();
		onChanged(message);
	};
	const title = snapshot?.api.Name ?? scope.apiId;
	return (
		<ContentLayout
			header={
				<Header
					variant="h1"
					description={snapshot?.api.Description}
					actions={
						<SpaceBetween direction="horizontal" size="xs">
							<Button
								iconName="refresh"
								ariaLabel="Refresh API"
								onClick={poll.reload}
								disabled={!enabled}
							/>
							<Button
								disabled={!enabled || !snapshot}
								onClick={() => setDialog("edit")}
							>
								Edit API
							</Button>
							<Button
								disabled={!enabled || !snapshot}
								onClick={() => setDialog("delete")}
							>
								Delete API
							</Button>
						</SpaceBetween>
					}
				>
					{title}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{poll.error && <Alert type="error">{poll.error}</Alert>}
				{!enabled ? (
					<Container header={<Header>API inventory</Header>}>
						<CodeBlock
							text={JSON.stringify(
								readonlyDetails ?? { ApiId: scope.apiId },
								null,
								2,
							)}
						/>
					</Container>
				) : !snapshot ? (
					<Box>
						{poll.loading
							? "Loading API configuration…"
							: "API configuration is unavailable."}
					</Box>
				) : (
					<>
						{section === "routes" || section === "apis" ? (
							http ? (
								<HttpRoutes
									scope={scope}
									snapshot={snapshot}
									onChanged={changed}
								/>
							) : (
								<RestResources
									scope={scope}
									snapshot={snapshot}
									enabled={enabled}
									onChanged={changed}
									onNavigate={onNavigate}
								/>
							)
						) : section === "integrations" && http ? (
							<HttpIntegrations
								scope={scope}
								snapshot={snapshot}
								onChanged={changed}
							/>
						) : section === "authorizers" ? (
							<Authorizers
								scope={scope}
								snapshot={snapshot}
								enabled={enabled}
								onChanged={changed}
							/>
						) : section === "stages" ? (
							<Stages
								scope={scope}
								snapshot={snapshot}
								enabled={enabled}
								onChanged={changed}
							/>
						) : section === "test" ? (
							<RequestTest
								scope={scope}
								snapshot={snapshot}
								enabled={enabled}
							/>
						) : (
							<SpaceBetween size="l">
								<Container header={<Header>API configuration</Header>}>
									<SpaceBetween size="s">
										<Box>API ID: {scope.apiId}</Box>
										<Box>ARN: {gatewayARN(scope)}</Box>
										<CodeBlock text={JSON.stringify(snapshot.api, null, 2)} />
									</SpaceBetween>
								</Container>
								<ConsoleTags
									tags={snapshot.api.Tags ?? {}}
									enabled={enabled}
									plan={(tags, removed) =>
										gatewayTagPlan(scope, gatewayARN(scope), tags, removed)
									}
									onChanged={() => changed("API tags saved.")}
								/>
							</SpaceBetween>
						)}
						{dialog === "edit" && (
							<ApiForm
								scope={scope}
								initial={snapshot.api}
								onClose={() => setDialog("")}
								onSaved={() => {
									setDialog("");
									changed("API settings saved.");
								}}
							/>
						)}
						{dialog === "delete" && (
							<ActionDialog
								title="Delete API"
								submit="Delete API"
								confirmation={snapshot.api.Name || scope.apiId}
								onClose={() => setDialog("")}
								onSaved={() => {
									changed("API deleted.");
									onDeleted();
								}}
								plan={async () => [
									{
										label: "Delete API",
										run: () =>
											apiCall(scope, http ? "DeleteApi" : "DeleteRestApi"),
									},
								]}
							>
								<Alert type="warning">
									This deletes this API, its routes, integrations, authorizers,
									stages and deployments. Requests to it will stop working.
								</Alert>
							</ActionDialog>
						)}
					</>
				)}
			</SpaceBetween>
		</ContentLayout>
	);
}
function HttpRoutes({
	scope,
	snapshot,
	onChanged,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	onChanged: (m: string) => void;
}) {
	const [dialog, setDialog] = useState(""),
		[selected, setSelected] = useState<GatewayRoute>();
	const done = (message: string) => {
		setDialog("");
		onChanged(message);
	};
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				Changes go live immediately on auto-deployed stages. Deploy manually
				managed stages to publish a new snapshot.
			</Alert>
			<GatewayTable
				items={snapshot.routes}
				trackBy="RouteId"
				header={
					<Header
						actions={
							<Button
								variant="primary"
								onClick={() => {
									setSelected(undefined);
									setDialog("edit");
								}}
							>
								Create route
							</Button>
						}
					>
						Routes
					</Header>
				}
				columnDefinitions={[
					{
						id: "key",
						header: "Route key",
						sortingField: "RouteKey",
						cell: (r) => r.RouteKey,
					},
					{
						id: "integration",
						header: "Integration",
						cell: (r) => r.Target || "None",
					},
					{
						id: "auth",
						header: "Authorization",
						cell: (r) => r.AuthorizationType ?? "NONE",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (r) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									variant="inline-link"
									onClick={() => {
										setSelected(r);
										setDialog("edit");
									}}
								>
									Edit
								</Button>
								<Button
									variant="inline-link"
									onClick={() => {
										setSelected(r);
										setDialog("delete");
									}}
								>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
			/>
			{dialog === "edit" && (
				<HttpRouteForm
					scope={scope}
					snapshot={snapshot}
					initial={selected}
					onClose={() => setDialog("")}
					onSaved={() => done("Route saved.")}
				/>
			)}
			{dialog === "delete" && selected && (
				<ActionDialog
					title="Delete route"
					submit="Delete route"
					confirmation={selected.RouteKey}
					onClose={() => setDialog("")}
					onSaved={() => done("Route deleted.")}
					plan={async () => [
						{
							label: "Delete route",
							run: () =>
								apiCall(scope, "DeleteRoute", { RouteId: selected.RouteId }),
						},
					]}
				>
					<Box>
						Auto-deployed stages will immediately stop serving this route.
					</Box>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function HttpIntegrations({
	scope,
	snapshot,
	onChanged,
}: {
	scope: GatewayScope;
	snapshot: GatewaySnapshot;
	onChanged: (m: string) => void;
}) {
	const [dialog, setDialog] = useState(""),
		[selected, setSelected] = useState<GatewayIntegration>();
	const done = (message: string) => {
		setDialog("");
		onChanged(message);
	};
	return (
		<SpaceBetween size="l">
			<GatewayTable
				items={snapshot.integrations}
				trackBy="IntegrationId"
				header={
					<Header
						actions={
							<Button
								variant="primary"
								onClick={() => {
									setSelected(undefined);
									setDialog("edit");
								}}
							>
								Create integration
							</Button>
						}
					>
						Integrations
					</Header>
				}
				columnDefinitions={[
					{ id: "id", header: "ID", cell: (i) => i.IntegrationId },
					{ id: "type", header: "Type", cell: (i) => i.IntegrationType },
					{ id: "uri", header: "Endpoint", cell: (i) => i.IntegrationUri },
					{
						id: "payload",
						header: "Payload format",
						cell: (i) => i.PayloadFormatVersion,
					},
					{
						id: "actions",
						header: "Actions",
						cell: (i) => (
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									variant="inline-link"
									onClick={() => {
										setSelected(i);
										setDialog("edit");
									}}
								>
									Edit
								</Button>
								<Button
									variant="inline-link"
									onClick={() => {
										setSelected(i);
										setDialog("delete");
									}}
								>
									Delete
								</Button>
							</SpaceBetween>
						),
					},
				]}
			/>
			{dialog === "edit" && (
				<IntegrationForm
					scope={scope}
					initial={selected}
					onClose={() => setDialog("")}
					onSaved={() => done("Integration saved.")}
				/>
			)}
			{dialog === "delete" && selected && (
				<ActionDialog
					title="Delete integration"
					submit="Delete integration"
					confirmation={selected.IntegrationId}
					onClose={() => setDialog("")}
					onSaved={() => done("Integration deleted.")}
					plan={async () => [
						{
							label: "Delete integration",
							run: () =>
								apiCall(scope, "DeleteIntegration", {
									IntegrationId: selected.IntegrationId,
								}),
						},
					]}
				>
					<Box>
						Remove this integration from its routes first. Routes and deployment
						snapshots may still reference it.
					</Box>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
