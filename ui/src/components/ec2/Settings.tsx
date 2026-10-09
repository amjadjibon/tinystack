import { Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { type ActionStep, jsonObject } from "#/lib/consoleActions";
import {
	type Ec2Row,
	type Ec2Scope,
	ec2Call,
	object,
	parentVpc,
	rows,
	strings,
	text,
} from "#/lib/ec2";
import { usePoll } from "#/lib/usePoll";
import { ActionDialog } from "../ActionDialog";
import {
	Choice,
	ChoiceStatus,
	Choices,
	Field,
	optionsFor,
	useChoices,
} from "./Fields";

export function Settings({
	scope,
	row,
	onClose,
	onSaved,
}: {
	scope: Ec2Scope;
	row: Ec2Row;
	onClose: () => void;
	onSaved: () => void;
}) {
	const section = scope.section;
	const choices = useChoices(
		scope,
		section === "gateways"
			? ["vpcs"]
			: section === "addresses"
				? ["instances"]
				: section === "endpoints"
					? ["subnets", "groups", "routes"]
					: [],
	);
	const dns = usePoll(
		async () =>
			section === "vpcs"
				? await Promise.all(
						["enableDnsSupport", "enableDnsHostnames"].map((Attribute) =>
							ec2Call(scope, "DescribeVpcAttribute", {
								VpcId: scope.id,
								Attribute,
							}),
						),
					)
				: [],
		0,
	);
	const [target, setTarget] = useState("");
	const [publicIp, setPublicIp] = useState(!!row.MapPublicIpOnLaunch);
	const [privateDns, setPrivateDns] = useState(!!row.PrivateDnsEnabled);
	const [support, setSupport] = useState<boolean>();
	const [hostnames, setHostnames] = useState<boolean>();
	const [policy, setPolicy] = useState(text(row, "PolicyDocument"));
	const [reset, setReset] = useState(false);
	const [routeIds, setRouteIds] = useState(strings(row, "RouteTableIds"));
	const [subnetIds, setSubnetIds] = useState(strings(row, "SubnetIds"));
	const [groupIds, setGroupIds] = useState(
		rows(row, "Groups").map((r) => text(r, "GroupId")),
	);
	const assoc = text(row, "AssociationId");
	const attached = parentVpc(row);
	const step = (
		label: string,
		operation: string,
		input: Ec2Row,
	): ActionStep => ({ label, run: () => ec2Call(scope, operation, input) });
	const vpc = text(row, "VpcId");
	const dnsSupport =
		support ?? !!object(dns.data?.[0] ?? {}, "EnableDnsSupport").Value;
	const dnsHostnames =
		hostnames ?? !!object(dns.data?.[1] ?? {}, "EnableDnsHostnames").Value;
	const diff = (field: string, next: string[], before: string[]) => ({
		[`Add${field}`]: next.filter((id) => !before.includes(id)),
		[`Remove${field}`]: before.filter((id) => !next.includes(id)),
	});
	return (
		<ActionDialog
			title={
				section === "gateways"
					? attached
						? "Detach internet gateway"
						: "Attach internet gateway"
					: section === "addresses"
						? assoc
							? "Disassociate Elastic IP address"
							: "Associate Elastic IP address"
						: section === "vpcs"
							? "Edit DNS settings"
							: section === "subnets"
								? "Edit subnet settings"
								: "Edit endpoint"
			}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (choices.loading || choices.error)
					throw new Error(
						"Resource choices must finish loading before saving.",
					);
				if (section === "vpcs") {
					if (dns.loading || dns.error)
						throw new Error("DNS settings must finish loading before saving.");
					if (dnsHostnames && !dnsSupport)
						throw new Error("DNS hostnames require DNS resolution.");
					const s = step("Save DNS resolution", "ModifyVpcAttribute", {
						VpcId: scope.id,
						EnableDnsSupport: { Value: dnsSupport },
					});
					const h = step("Save DNS hostnames", "ModifyVpcAttribute", {
						VpcId: scope.id,
						EnableDnsHostnames: { Value: dnsHostnames },
					});
					return dnsSupport ? [s, h] : [h, s];
				}
				if (section === "subnets")
					return [
						step("Save public IPv4 setting", "ModifySubnetAttribute", {
							SubnetId: scope.id,
							MapPublicIpOnLaunch: { Value: publicIp },
						}),
					];
				if (section === "gateways") {
					if (!attached && !target) throw new Error("Choose a VPC.");
					return [
						step(
							attached ? "Detach gateway" : "Attach gateway",
							attached ? "DetachInternetGateway" : "AttachInternetGateway",
							{ InternetGatewayId: scope.id, VpcId: attached || target },
						),
					];
				}
				if (section === "addresses") {
					if (!assoc && !target) throw new Error("Choose an instance.");
					return [
						step(
							assoc ? "Disassociate address" : "Associate address",
							assoc ? "DisassociateAddress" : "AssociateAddress",
							assoc
								? { AssociationId: assoc }
								: {
										AllocationId: scope.id,
										InstanceId: target,
										AllowReassociation: false,
									},
						),
					];
				}
				if (!reset && policy.trim()) jsonObject(policy, "Endpoint policy");
				return [
					step("Save endpoint", "ModifyVpcEndpoint", {
						VpcEndpointId: scope.id,
						...(reset
							? { ResetPolicy: true }
							: policy.trim()
								? { PolicyDocument: policy }
								: {}),
						...(text(row, "VpcEndpointType") === "Gateway"
							? diff("RouteTableIds", routeIds, strings(row, "RouteTableIds"))
							: {
									...diff("SubnetIds", subnetIds, strings(row, "SubnetIds")),
									...diff(
										"SecurityGroupIds",
										groupIds,
										rows(row, "Groups").map((r) => text(r, "GroupId")),
									),
									PrivateDnsEnabled: privateDns,
								}),
					}),
				];
			}}
		>
			<SpaceBetween size="l">
				<ChoiceStatus {...choices} />
				{section === "vpcs" && (
					<>
						<ChoiceStatus {...dns} />
						<Checkbox
							checked={dnsSupport}
							onChange={({ detail }) => setSupport(detail.checked)}
						>
							Enable DNS resolution
						</Checkbox>
						<Checkbox
							checked={dnsHostnames}
							onChange={({ detail }) => setHostnames(detail.checked)}
						>
							Enable DNS hostnames
						</Checkbox>
					</>
				)}
				{section === "subnets" && (
					<Checkbox
						checked={publicIp}
						onChange={({ detail }) => setPublicIp(detail.checked)}
					>
						Auto-assign public IPv4 addresses to new instances
					</Checkbox>
				)}
				{section === "gateways" &&
					(attached ? (
						<p>
							Detach {scope.id} from {attached}. Dependencies must be removed
							first.
						</p>
					) : (
						<Choice
							label="VPC"
							value={target}
							onChange={setTarget}
							options={optionsFor("vpcs", choices.items("vpcs"))}
						/>
					))}
				{section === "addresses" &&
					(assoc ? (
						<p>
							Disassociate {text(row, "PublicIp")} from{" "}
							{text(row, "InstanceId")}.
						</p>
					) : (
						<Choice
							label="Instance"
							value={target}
							onChange={setTarget}
							options={optionsFor(
								"instances",
								choices
									.items("instances")
									.filter(
										(r) => text(object(r, "State"), "Name") !== "terminated",
									),
							)}
						/>
					))}
				{section === "endpoints" && (
					<>
						{text(row, "VpcEndpointType") === "Gateway" ? (
							<Choices
								label="Route tables"
								value={routeIds}
								onChange={setRouteIds}
								options={optionsFor(
									"routes",
									choices
										.items("routes")
										.filter((r) => text(r, "VpcId") === vpc),
								)}
							/>
						) : (
							<>
								<Choices
									label="Subnets"
									value={subnetIds}
									onChange={setSubnetIds}
									options={optionsFor(
										"subnets",
										choices
											.items("subnets")
											.filter((r) => text(r, "VpcId") === vpc),
									)}
								/>
								<Choices
									label="Security groups"
									value={groupIds}
									onChange={setGroupIds}
									options={optionsFor(
										"groups",
										choices
											.items("groups")
											.filter((r) => text(r, "VpcId") === vpc),
									)}
								/>
								<Checkbox
									checked={privateDns}
									onChange={({ detail }) => setPrivateDns(detail.checked)}
								>
									Enable private DNS
								</Checkbox>
							</>
						)}
						<Checkbox
							checked={reset}
							onChange={({ detail }) => setReset(detail.checked)}
						>
							Reset endpoint policy to its default
						</Checkbox>
						{!reset && (
							<Field
								label="Endpoint policy (JSON)"
								multiline
								value={policy}
								onChange={setPolicy}
							/>
						)}
					</>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}

export function ProfileSettings({
	scope,
	resources,
	onClose,
	onSaved,
}: {
	scope: Ec2Scope;
	resources: {
		service: string;
		type: string;
		account: string;
		name: string;
		arn: string;
	}[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const [draft, setProfile] = useState<string>();
	const association = usePoll(
		async () =>
			rows(
				await ec2Call(scope, "DescribeIamInstanceProfileAssociations", {
					Filters: [{ Name: "instance-id", Values: [scope.id] }],
				}),
				"IamInstanceProfileAssociations",
			)[0],
		0,
	);
	const profile =
		draft ?? text(object(association.data ?? {}, "IamInstanceProfile"), "Arn");
	return (
		<ActionDialog
			title="Manage IAM instance profile"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (association.loading || association.error)
					throw new Error("The current association must finish loading.");
				const id = text(association.data ?? {}, "AssociationId");
				if (!profile && !id) throw new Error("Choose an instance profile.");
				return [
					{
						label: "Save instance profile",
						run: () =>
							ec2Call(
								scope,
								profile
									? id
										? "ReplaceIamInstanceProfileAssociation"
										: "AssociateIamInstanceProfile"
									: "DisassociateIamInstanceProfile",
								profile
									? {
											...(id
												? { AssociationId: id }
												: { InstanceId: scope.id }),
											IamInstanceProfile: { Arn: profile },
										}
									: { AssociationId: id },
							),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<ChoiceStatus {...association} />
				<p>
					Current profile:{" "}
					{text(object(association.data ?? {}, "IamInstanceProfile"), "Arn") ||
						"None"}
				</p>
				<Choice
					label="IAM instance profile"
					value={profile}
					onChange={setProfile}
					optional
					options={resources
						.filter(
							(r) =>
								r.service === "iam" &&
								r.type === "instance-profile" &&
								r.account === scope.account,
						)
						.map((r) => ({ label: r.name, value: r.arn }))}
					description="Choose None to remove the current association. Create instance profiles in IAM."
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
