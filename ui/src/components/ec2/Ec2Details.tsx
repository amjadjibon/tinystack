import {
	Alert,
	Box,
	Button,
	Container,
	Header,
	KeyValuePairs,
	Link,
	SpaceBetween,
	StatusIndicator,
	Tabs,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { Resource } from "#/lib/api";
import {
	type Ec2Row,
	type Ec2Scope,
	type Ec2Section,
	ec2Call,
	ec2Delete,
	ec2Describe,
	ec2Href,
	ec2Kinds,
	ec2TagPlan,
	object,
	parentVpc,
	rowName,
	rowState,
	rows,
	tagsOf,
	text,
} from "#/lib/ec2";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { ActionDialog } from "../ActionDialog";
import { CodeBlock } from "../CodeBlock";
import { ConsoleTags } from "../ConsoleTags";
import { Ec2Table } from "./Ec2Table";
import { ChoiceStatus, useChoices } from "./Fields";
import { NetworkAcl } from "./NetworkAcl";
import { Routes } from "./Routes";
import { SecurityRules } from "./SecurityRules";
import { ProfileSettings, Settings } from "./Settings";

export function Ec2Details({
	scope,
	enabled,
	fallback,
	resources,
	onChanged,
	onDeleted,
}: {
	scope: Ec2Scope;
	enabled: boolean;
	fallback?: Ec2Row;
	resources: Resource[];
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const kind = ec2Kinds[scope.section],
		follow = useFollow();
	const poll = usePoll(async () => (enabled ? ec2Describe(scope) : fallback));
	const row = poll.data ?? fallback;
	const [dialog, setDialog] = useState("");
	const changed = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	const link = (section: Ec2Section, id: string) =>
		id ? (
			<Link
				href={ec2Href({
					...scope,
					area:
						section === "instances" ||
						section === "images" ||
						section === "keys"
							? "ec2"
							: "vpc",
					section,
					id,
					vpc: undefined,
				})}
				onFollow={follow}
			>
				{id}
			</Link>
		) : (
			"—"
		);
	const state = row ? rowState(row) : "";
	const terminated = state === "terminated";
	return (
		<SpaceBetween size="l">
			<Header
				variant="h1"
				description={rowName(row ?? {}) || kind.singular}
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button
							iconName="refresh"
							ariaLabel="Refresh resource"
							onClick={poll.reload}
						/>
						{scope.section === "instances" && (
							<>
								<Button
									disabled={!enabled || state !== "stopped"}
									onClick={() => setDialog("StartInstances")}
								>
									Start
								</Button>
								<Button
									disabled={!enabled || state !== "running"}
									onClick={() => setDialog("StopInstances")}
								>
									Stop
								</Button>
								<Button
									disabled={!enabled || state !== "running"}
									onClick={() => setDialog("RebootInstances")}
								>
									Reboot
								</Button>
								<Button
									disabled={!enabled || terminated}
									onClick={() => setDialog("profile")}
								>
									Manage IAM profile
								</Button>
							</>
						)}
						{["vpcs", "subnets", "gateways", "addresses", "endpoints"].includes(
							scope.section,
						) && (
							<Button
								disabled={
									!enabled ||
									!row ||
									!!(
										scope.section === "addresses" &&
										row.NetworkInterfaceId &&
										!row.InstanceId
									)
								}
								onClick={() => setDialog("settings")}
							>
								{scope.section === "gateways"
									? parentVpc(row ?? {})
										? "Detach from VPC"
										: "Attach to VPC"
									: scope.section === "addresses"
										? row?.AssociationId
											? "Disassociate address"
											: "Associate address"
										: "Edit settings"}
							</Button>
						)}
						{kind.delete && (
							<Button
								disabled={
									!enabled ||
									!row ||
									terminated ||
									state === "deleted" ||
									!!(scope.section === "acls" && row.IsDefault) ||
									!!(
										scope.section === "groups" && row.GroupName === "default"
									) ||
									!!(
										scope.section === "routes" &&
										rows(row, "Associations").some((r) => r.Main)
									)
								}
								onClick={() => setDialog("delete")}
							>
								{scope.section === "instances"
									? "Terminate"
									: scope.section === "addresses"
										? "Release"
										: "Delete"}
							</Button>
						)}
					</SpaceBetween>
				}
			>
				{scope.id}
			</Header>
			{poll.error && (
				<Alert type="error" header="Cannot load resource">
					{poll.error}
				</Alert>
			)}
			{!row ? (
				<Box>
					{poll.loading
						? "Loading resource…"
						: "Resource not found in this account and Region."}
				</Box>
			) : (
				<>
					<Container header={<Header variant="h2">Details</Header>}>
						<KeyValuePairs
							columns={3}
							items={[
								{ label: `${kind.singular} ID`, value: scope.id || "—" },
								{ label: "Name", value: rowName(row) || "—" },
								{
									label: "Status",
									value: state ? (
										<StatusIndicator
											type={
												state === "running" ||
												state === "available" ||
												state === "active"
													? "success"
													: state === "failed"
														? "error"
														: "info"
											}
										>
											{state}
										</StatusIndicator>
									) : (
										"—"
									),
								},
								...kind.columns.map(([key, label]) => ({
									label,
									value:
										key === "VpcId"
											? link("vpcs", text(row, key))
											: key === "SubnetId"
												? link("subnets", text(row, key))
												: key === "InstanceId"
													? link("instances", text(row, key))
													: row[key] === undefined
														? "—"
														: typeof row[key] === "boolean"
															? row[key]
																? "Yes"
																: "No"
															: text(row, key),
								})),
								{ label: "Account", value: scope.account },
								{ label: "Region", value: scope.region },
							]}
						/>
					</Container>
					<Tabs
						tabs={[
							...(scope.section === "vpcs"
								? [
										{
											id: "resources",
											label: "Resources",
											content: (
												<VpcResources
													scope={scope}
													enabled={enabled}
													resources={resources}
												/>
											),
										},
									]
								: []),
							...(scope.section === "groups"
								? [
										{
											id: "rules",
											label: "Security group rules",
											content: (
												<SecurityRules
													scope={scope}
													row={row}
													enabled={enabled}
													onChanged={changed}
												/>
											),
										},
									]
								: []),
							...(scope.section === "routes"
								? [
										{
											id: "routes",
											label: "Routes and associations",
											content: (
												<Routes
													scope={scope}
													row={row}
													enabled={enabled}
													onChanged={changed}
												/>
											),
										},
									]
								: []),
							...(scope.section === "acls"
								? [
										{
											id: "rules",
											label: "Rules and associations",
											content: (
												<NetworkAcl
													scope={scope}
													row={row}
													enabled={enabled}
													onChanged={changed}
												/>
											),
										},
									]
								: []),
							...(scope.section === "instances"
								? [
										{
											id: "network",
											label: "Networking",
											content: (
												<Container
													header={<Header variant="h2">Networking</Header>}
												>
													<KeyValuePairs
														columns={2}
														items={[
															{
																label: "VPC",
																value: link("vpcs", text(row, "VpcId")),
															},
															{
																label: "Subnet",
																value: link("subnets", text(row, "SubnetId")),
															},
															{
																label: "Security groups",
																value: (
																	<SpaceBetween size="xs">
																		{rows(row, "SecurityGroups").map((r) => (
																			<span key={text(r, "GroupId")}>
																				{link("groups", text(r, "GroupId"))}
																			</span>
																		))}
																	</SpaceBetween>
																),
															},
															{
																label: "Availability Zone",
																value:
																	text(
																		object(row, "Placement"),
																		"AvailabilityZone",
																	) || "—",
															},
															{
																label: "Private DNS",
																value: text(row, "PrivateDnsName") || "—",
															},
															{
																label: "Public DNS",
																value: text(row, "PublicDnsName") || "—",
															},
															{
																label: "Key pair",
																value: text(row, "KeyName") || "None",
															},
															{
																label: "IAM instance profile",
																value:
																	text(
																		object(row, "IamInstanceProfile"),
																		"Arn",
																	) || "None",
															},
														]}
													/>
												</Container>
											),
										},
										{
											id: "status",
											label: "Status checks",
											content: (
												<InstanceStatus scope={scope} enabled={enabled} />
											),
										},
									]
								: []),
							{
								id: "metadata",
								label:
									scope.section === "keys"
										? "Public key and metadata"
										: "Resource metadata",
								content: (
									<Container
										header={
											<Header variant="h2">{kind.singular} metadata</Header>
										}
									>
										<SpaceBetween size="l">
											{scope.section === "interfaces" && (
												<Alert type="info">
													Network interfaces are derived from instances, NAT
													gateways, and interface endpoints. Manage their parent
													resource to change them.
												</Alert>
											)}
											{scope.section === "keys" && (
												<Alert type="info">
													Private key material is available only when a key pair
													is created. This page contains the public key.
												</Alert>
											)}
											<CodeBlock text={JSON.stringify(row, null, 2)} />
										</SpaceBetween>
									</Container>
								),
							},
							...(!["images", "interfaces"].includes(scope.section)
								? [
										{
											id: "tags",
											label: "Tags",
											content: (
												<ConsoleTags
													tags={tagsOf(row)}
													enabled={enabled}
													plan={(next, removed) =>
														ec2TagPlan(scope, next, removed)
													}
													onChanged={changed}
												/>
											),
										},
									]
								: []),
						]}
					/>
					{dialog === "settings" && (
						<Settings
							scope={scope}
							row={row}
							onClose={() => setDialog("")}
							onSaved={changed}
						/>
					)}
					{dialog === "profile" && (
						<ProfileSettings
							scope={scope}
							resources={resources}
							onClose={() => setDialog("")}
							onSaved={changed}
						/>
					)}
					{dialog === "delete" && (
						<ActionDialog
							title={`${scope.section === "instances" ? "Terminate" : scope.section === "addresses" ? "Release" : "Delete"} ${kind.singular === "VPC" ? "VPC" : kind.singular.toLowerCase()}`}
							submit={
								scope.section === "instances"
									? "Terminate instance"
									: scope.section === "addresses"
										? "Release address"
										: "Delete"
							}
							confirmation={scope.id}
							onClose={() => setDialog("")}
							onSaved={onDeleted}
							plan={async () => [
								{ label: kind.delete || "Delete", run: () => ec2Delete(scope) },
							]}
						>
							<p>
								{scope.id} will be{" "}
								{scope.section === "instances"
									? "terminated"
									: scope.section === "addresses"
										? "released"
										: "deleted"}
								. Remove dependent resources and associations first.
							</p>
						</ActionDialog>
					)}
					{["StartInstances", "StopInstances", "RebootInstances"].includes(
						dialog,
					) && (
						<ActionDialog
							title={`${dialog.replace("Instances", "")} instance`}
							submit={dialog.replace("Instances", "")}
							onClose={() => setDialog("")}
							onSaved={changed}
							plan={async () => [
								{
									label: dialog,
									run: () =>
										ec2Call(scope, dialog, { InstanceIds: [scope.id] }),
								},
							]}
						>
							<p>
								Apply this action to {scope.id}. TinyStack updates its instance
								record immediately.
							</p>
						</ActionDialog>
					)}
				</>
			)}
		</SpaceBetween>
	);
}

function InstanceStatus({
	scope,
	enabled,
}: {
	scope: Ec2Scope;
	enabled: boolean;
}) {
	const poll = usePoll(async () =>
		enabled
			? rows(
					await ec2Call(scope, "DescribeInstanceStatus", {
						InstanceIds: [scope.id],
						IncludeAllInstances: true,
					}),
					"InstanceStatuses",
				)
			: [],
	);
	return (
		<SpaceBetween size="l">
			<Alert type="info">
				These status checks describe TinyStack instance records. They do not
				measure a running VM.
			</Alert>
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Ec2Table
				title="Status checks"
				items={poll.data ?? []}
				loading={poll.loading}
				trackBy="InstanceId"
				columns={[
					{ id: "id", header: "Instance", cell: (r) => text(r, "InstanceId") },
					{
						id: "system",
						header: "System status",
						cell: (r) => text(object(r, "SystemStatus"), "Status"),
					},
					{
						id: "instance",
						header: "Instance status",
						cell: (r) => text(object(r, "InstanceStatus"), "Status"),
					},
				]}
			/>
		</SpaceBetween>
	);
}
function VpcResources({
	scope,
	enabled,
	resources,
}: {
	scope: Ec2Scope;
	enabled: boolean;
	resources: Resource[];
}) {
	const sections: Ec2Section[] = [
		"subnets",
		"groups",
		"routes",
		"gateways",
		"nat",
		"acls",
		"endpoints",
		"instances",
	];
	const poll = useChoices(scope, enabled ? sections : []),
		follow = useFollow();
	return (
		<Container header={<Header variant="h2">Resources in this VPC</Header>}>
			<SpaceBetween size="l">
				<ChoiceStatus {...poll} />
				<KeyValuePairs
					columns={3}
					items={sections.map((section) => ({
						label: ec2Kinds[section].title,
						value: (
							<Link
								href={ec2Href({
									...scope,
									area: section === "instances" ? "ec2" : "vpc",
									section,
									id: undefined,
									vpc: scope.id,
								})}
								onFollow={follow}
							>
								{enabled
									? poll.items(section).filter((r) => parentVpc(r) === scope.id)
											.length
									: resources.filter(
											(r) =>
												r.service === "ec2" &&
												r.account === scope.account &&
												r.region === scope.region &&
												r.type === ec2Kinds[section].type &&
												r.details?.VPCID === scope.id,
										).length}
							</Link>
						),
					}))}
				/>
			</SpaceBetween>
		</Container>
	);
}
