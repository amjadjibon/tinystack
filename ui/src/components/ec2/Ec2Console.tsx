import {
	Alert,
	Box,
	Button,
	CopyToClipboard,
	Flashbar,
	Link,
	Modal,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useEffect, useRef, useState } from "react";
import type { Resource } from "#/lib/api";
import {
	type Ec2Row,
	type Ec2Scope,
	type Ec2Section,
	ec2Href,
	ec2Kinds,
	ec2List,
	inventoryRow,
	parentVpc,
	rowId,
	rowName,
	rowState,
	text,
} from "#/lib/ec2";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CodeBlock } from "../CodeBlock";
import { ConsoleLayout } from "../Console";
import { CreateResource } from "./CreateResource";
import { Ec2Details } from "./Ec2Details";
import { Ec2Table } from "./Ec2Table";
import { LaunchInstances } from "./LaunchInstances";

export function Ec2Console({
	search,
	area,
}: {
	search: { section: Ec2Section; id: string; vpc: string };
	area: "ec2" | "vpc";
}) {
	const { canWrite, account, region, inventory, capabilities } = useScope(),
		navigate = useNavigate();
	const scope: Ec2Scope = {
			account,
			region,
			area,
			section: search.section,
			id: search.id,
			vpc: search.vpc,
		},
		kind = ec2Kinds[scope.section];
	const context = `${account}/${region}/${area}/${search.section}/${search.id}/${search.vpc}`;
	const mounted = useRef(true),
		current = useRef(context);
	current.current = context;
	useEffect(() => {
		mounted.current = true;
		return () => {
			mounted.current = false;
		};
	}, []);
	const [notice, setNotice] = useState<{ scope: string; message: string }>();
	const [key, setKey] = useState<{
		account: string;
		region: string;
		material: string;
		name: string;
	}>();
	// biome-ignore lint/correctness/useExhaustiveDependencies: private material belongs only to this account and Region.
	useEffect(() => {
		setKey(undefined);
		setNotice(undefined);
	}, [account, region]);
	const enabled =
		!!capabilities.data?.ec2 &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	const resources = (inventory.data ?? []).filter(
		(r) => r.service === "ec2" && r.account === account && r.region === region,
	);
	const safe = () => mounted.current && current.current === context;
	const changed = (message = "Resource updated.") => {
		inventory.reload();
		if (safe()) setNotice({ scope: `${account}/${region}`, message });
	};
	const go = (id = "") => {
		if (safe())
			return navigate({
				to: `/services/${area}`,
				search: {
					account,
					region,
					section: scope.section,
					id,
					vpc: search.vpc,
				},
			});
	};
	const href = (section: Ec2Section, targetArea: Ec2Scope["area"] = area) =>
		ec2Href({
			...scope,
			area: targetArea,
			section,
			id: undefined,
			vpc: undefined,
		});
	const sections: Ec2Section[] =
		area === "ec2"
			? ["instances", "images", "keys", "groups", "addresses", "interfaces"]
			: [
					"vpcs",
					"subnets",
					"routes",
					"gateways",
					"addresses",
					"nat",
					"groups",
					"acls",
					"endpoints",
					"interfaces",
				];
	return (
		<ConsoleLayout
			contentType={scope.id ? "default" : "table"}
			navigationHeader={{
				text: area === "ec2" ? "EC2" : "VPC",
				href: href(area === "ec2" ? "instances" : "vpcs"),
			}}
			activeHref={href(scope.section)}
			navigationItems={[
				...sections.map((section) => ({
					type: "link" as const,
					text: ec2Kinds[section].title,
					href: href(section),
				})),
				{ type: "divider" },
				{
					type: "link",
					text: area === "ec2" ? "VPC console" : "EC2 console",
					href: href(
						area === "ec2" ? "vpcs" : "instances",
						area === "ec2" ? "vpc" : "ec2",
					),
				},
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "Request inspector", href: "/requests" },
			]}
			breadcrumbs={[
				{
					text: area === "ec2" ? "EC2" : "VPC",
					href: href(area === "ec2" ? "instances" : "vpcs"),
				},
				{ text: kind.title, href: ec2Href({ ...scope, id: undefined }) },
				...(scope.id ? [{ text: scope.id, href: ec2Href(scope) }] : []),
			]}
			content={
				<SpaceBetween size="l">
					{notice?.scope === `${account}/${region}` && (
						<Flashbar
							items={[
								{
									id: "ec2-success",
									type: "success",
									content: notice.message,
									dismissible: true,
									onDismiss: () => setNotice(undefined),
								},
							]}
						/>
					)}
					<Alert type="info">
						TinyStack models EC2 instances and VPC resources. Instances do not
						boot VMs, accept SSH connections, or execute user data. Network
						settings are stored as records and do not create real networking.
					</Alert>
					{(inventory.error || capabilities.error) && (
						<Alert type="error">{inventory.error || capabilities.error}</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info" header="Management is read-only">
							Connect a root session for the selected account using Console
							session in the top bar, or use signed AWS CLI/SDK requests.
						</Alert>
					)}
					{capabilities.data && !capabilities.data.ec2 && (
						<Alert type="warning">EC2 is not enabled on this server.</Alert>
					)}
					<Workspace
						key={`${context}/${enabled}`}
						scope={scope}
						resources={resources}
						iamResources={inventory.data ?? []}
						enabled={enabled}
						onRefresh={inventory.reload}
						onChanged={changed}
						onGo={go}
						onCreated={(id, result) => {
							inventory.reload();
							if (!safe()) return;
							setNotice({
								scope: `${account}/${region}`,
								message:
									scope.section === "instances"
										? `${result.Count ?? 1} instance(s) launched.`
										: `${kind.singular} created.`,
							});
							if (result.KeyMaterial)
								setKey({
									account,
									region,
									name: text(result, "KeyName"),
									material: text(result, "KeyMaterial"),
								});
							go(id);
						}}
					/>
					{key?.account === account && key.region === region && (
						<Modal
							visible
							size="large"
							header={`Private key: ${key.name}`}
							closeAriaLabel="Close private key"
							onDismiss={() => setKey(undefined)}
							footer={
								<Box float="right">
									<Button variant="primary" onClick={() => setKey(undefined)}>
										Done
									</Button>
								</Box>
							}
						>
							<SpaceBetween size="l">
								<Alert type="info">
									Save this private key now. TinyStack will not return it again.
								</Alert>
								<CopyToClipboard
									textToCopy={key.material}
									copyButtonText="Copy private key"
									copySuccessText="Private key copied"
									copyErrorText="Copy failed"
								/>
								<CodeBlock text={key.material} />
							</SpaceBetween>
						</Modal>
					)}
				</SpaceBetween>
			}
		/>
	);
}
function Workspace({
	scope,
	enabled,
	resources,
	iamResources,
	onRefresh,
	onChanged,
	onGo,
	onCreated,
}: {
	scope: Ec2Scope;
	enabled: boolean;
	resources: Resource[];
	iamResources: Resource[];
	onRefresh: () => void;
	onChanged: (message?: string) => void;
	onGo: (id?: string) => void;
	onCreated: (id: string, result: Ec2Row) => void;
}) {
	const kind = ec2Kinds[scope.section],
		follow = useFollow(),
		[creating, setCreating] = useState(false);
	const fallback = resources
		.filter((r) => r.type === kind.type)
		.map(inventoryRow);
	const poll = usePoll(async () =>
		enabled && !scope.id ? ec2List(scope, scope.section) : undefined,
	);
	if (scope.id)
		return (
			<Ec2Details
				scope={scope}
				enabled={enabled}
				fallback={fallback.find((r) => rowId(scope.section, r) === scope.id)}
				resources={iamResources}
				onChanged={onChanged}
				onDeleted={() => {
					onChanged(
						`${kind.singular} ${scope.id} ${scope.section === "instances" ? "terminated" : "deleted"}.`,
					);
					onGo();
				}}
			/>
		);
	const all = enabled ? (poll.data ?? []) : fallback;
	const items = scope.vpc ? all.filter((r) => parentVpc(r) === scope.vpc) : all;
	return (
		<SpaceBetween size="l">
			{poll.error && (
				<Alert type="error" header="Cannot load resources">
					{poll.error}
				</Alert>
			)}
			{scope.vpc && (
				<Alert type="info">
					Showing resources in{" "}
					<Link
						href={ec2Href({
							...scope,
							area: "vpc",
							section: "vpcs",
							id: scope.vpc,
							vpc: undefined,
						})}
						onFollow={follow}
					>
						{scope.vpc}
					</Link>
					.{" "}
					<Link href={ec2Href({ ...scope, vpc: undefined })} onFollow={follow}>
						Clear VPC filter
					</Link>
				</Alert>
			)}
			<Ec2Table
				page
				title={kind.title}
				description={
					scope.section === "images"
						? "Fixed regional AMI catalogue for modeled instance launches."
						: undefined
				}
				items={items}
				loading={enabled && poll.loading}
				trackBy={kind.id}
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							iconName="refresh"
							ariaLabel="Refresh resources"
							onClick={() => {
								poll.reload();
								onRefresh();
							}}
						/>
						{kind.create && (
							<Button
								variant="primary"
								disabled={!enabled}
								onClick={() => setCreating(true)}
							>
								{scope.section === "instances"
									? "Launch instances"
									: scope.section === "addresses"
										? "Allocate Elastic IP address"
										: `Create ${kind.singular === "VPC" ? "VPC" : kind.singular.toLowerCase()}`}
							</Button>
						)}
					</SpaceBetween>
				}
				columns={[
					{ id: "name", header: "Name", cell: (r) => rowName(r) || "—" },
					{
						id: "id",
						header: `${kind.singular} ID`,
						sortingField: kind.id,
						cell: (r) => (
							<Link
								href={ec2Href({ ...scope, id: rowId(scope.section, r) })}
								onFollow={follow}
							>
								{rowId(scope.section, r)}
							</Link>
						),
					},
					{
						id: "state",
						header: "Status",
						cell: (r) =>
							rowState(r) ? (
								<StatusIndicator
									type={
										["running", "available", "active"].includes(rowState(r))
											? "success"
											: rowState(r) === "failed"
												? "error"
												: "info"
									}
								>
									{rowState(r)}
								</StatusIndicator>
							) : scope.section === "gateways" ? (
								parentVpc(r) ? (
									"Attached"
								) : (
									"Detached"
								)
							) : (
								"—"
							),
					},
					...kind.columns.map(([key, label]) => ({
						id: key,
						header: label,
						sortingField: key,
						cell: (r: Ec2Row) =>
							key === "VpcId" && r.VpcId ? (
								<Link
									href={ec2Href({
										...scope,
										area: "vpc",
										section: "vpcs",
										id: text(r, "VpcId"),
										vpc: undefined,
									})}
									onFollow={follow}
								>
									{text(r, "VpcId")}
								</Link>
							) : r[key] === undefined ? (
								"—"
							) : typeof r[key] === "boolean" ? (
								r[key] ? (
									"Yes"
								) : (
									"No"
								)
							) : (
								text(r, key)
							),
					})),
				]}
			/>
			{creating &&
				enabled &&
				(scope.section === "instances" ? (
					<LaunchInstances
						scope={scope}
						resources={iamResources}
						onClose={() => setCreating(false)}
						onSaved={(id, count) => onCreated(id, { Count: count })}
					/>
				) : (
					<CreateResource
						scope={scope}
						onClose={() => setCreating(false)}
						onSaved={onCreated}
					/>
				))}
		</SpaceBetween>
	);
}
