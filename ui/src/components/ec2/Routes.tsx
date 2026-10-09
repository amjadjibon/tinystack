import { Button, Link, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import {
	type Ec2Row,
	type Ec2Scope,
	ec2Call,
	ec2Href,
	ec2List,
	parentVpc,
	rows,
	text,
} from "#/lib/ec2";
import { useFollow } from "#/lib/useFollow";
import { ActionDialog } from "../ActionDialog";
import { Ec2Table } from "./Ec2Table";
import { Choice, ChoiceStatus, Field, optionsFor, useChoices } from "./Fields";

export function Routes({
	scope,
	row,
	enabled,
	onChanged,
}: {
	scope: Ec2Scope;
	row: Ec2Row;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [edit, setEdit] = useState<Ec2Row>(),
		[remove, setRemove] = useState<Ec2Row>(),
		[associate, setAssociate] = useState(false),
		[main, setMain] = useState(false),
		[detach, setDetach] = useState<Ec2Row>();
	const follow = useFollow(),
		changed = () => {
			setEdit(undefined);
			setRemove(undefined);
			setAssociate(false);
			setMain(false);
			setDetach(undefined);
			onChanged();
		};
	return (
		<SpaceBetween size="l">
			<Ec2Table
				title="Routes"
				items={rows(row, "Routes")}
				trackBy={(r) =>
					text(r, "DestinationCidrBlock") || text(r, "DestinationPrefixListId")
				}
				actions={
					<Button disabled={!enabled} onClick={() => setEdit({})}>
						Add route
					</Button>
				}
				columns={[
					{
						id: "destination",
						header: "Destination",
						cell: (r) =>
							text(r, "DestinationCidrBlock") ||
							text(r, "DestinationPrefixListId"),
					},
					{
						id: "target",
						header: "Target",
						cell: (r) =>
							text(r, "GatewayId") ||
							text(r, "NatGatewayId") ||
							text(r, "InstanceId"),
					},
					{ id: "state", header: "Status", cell: (r) => text(r, "State") },
					{ id: "origin", header: "Origin", cell: (r) => text(r, "Origin") },
					{
						id: "actions",
						header: "Actions",
						cell: (r) =>
							text(r, "GatewayId") === "local" || r.DestinationPrefixListId ? (
								"Managed route"
							) : (
								<SpaceBetween direction="horizontal" size="xs">
									<Button disabled={!enabled} onClick={() => setEdit(r)}>
										Edit
									</Button>
									<Button disabled={!enabled} onClick={() => setRemove(r)}>
										Delete
									</Button>
								</SpaceBetween>
							),
					},
				]}
			/>
			<Ec2Table
				title="Subnet and main associations"
				description="Subnets without an explicit association use the VPC's main route table."
				items={rows(row, "Associations")}
				trackBy="RouteTableAssociationId"
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button disabled={!enabled} onClick={() => setAssociate(true)}>
							Associate subnet
						</Button>
						<Button
							disabled={
								!enabled || rows(row, "Associations").some((r) => r.Main)
							}
							onClick={() => setMain(true)}
						>
							Set as main route table
						</Button>
					</SpaceBetween>
				}
				columns={[
					{
						id: "id",
						header: "Association ID",
						cell: (r) => text(r, "RouteTableAssociationId"),
					},
					{
						id: "subnet",
						header: "Subnet",
						cell: (r) =>
							r.SubnetId ? (
								<Link
									href={ec2Href({
										...scope,
										section: "subnets",
										id: text(r, "SubnetId"),
									})}
									onFollow={follow}
								>
									{text(r, "SubnetId")}
								</Link>
							) : (
								"—"
							),
					},
					{ id: "main", header: "Main", cell: (r) => (r.Main ? "Yes" : "No") },
					{
						id: "actions",
						header: "Actions",
						cell: (r) => (
							<Button
								disabled={!enabled || !!r.Main}
								onClick={() => setDetach(r)}
							>
								Disassociate
							</Button>
						),
					},
				]}
			/>
			{main && (
				<ActionDialog
					title="Set main route table"
					onClose={() => setMain(false)}
					onSaved={changed}
					plan={async () => {
						const all = await ec2List(scope, "routes"),
							association = all
								.filter((r) => text(r, "VpcId") === text(row, "VpcId"))
								.flatMap((r) => rows(r, "Associations"))
								.find((r) => r.Main);
						if (!association)
							throw new Error(
								"The VPC's main route table association was not found.",
							);
						return [
							{
								label: "Replace main route table",
								run: () =>
									ec2Call(scope, "ReplaceRouteTableAssociation", {
										AssociationId: text(association, "RouteTableAssociationId"),
										RouteTableId: scope.id,
									}),
							},
						];
					}}
				>
					<p>
						Subnets without an explicit route table association will use{" "}
						{scope.id}.
					</p>
				</ActionDialog>
			)}
			{edit && (
				<RouteForm
					scope={scope}
					vpc={text(row, "VpcId")}
					route={edit}
					onClose={() => setEdit(undefined)}
					onSaved={changed}
				/>
			)}
			{associate && (
				<AssociationForm
					scope={scope}
					vpc={text(row, "VpcId")}
					mode="route"
					onClose={() => setAssociate(false)}
					onSaved={changed}
				/>
			)}
			{remove && (
				<ActionDialog
					title="Delete route"
					submit="Delete route"
					confirmation={text(remove, "DestinationCidrBlock")}
					onClose={() => setRemove(undefined)}
					onSaved={changed}
					plan={async () => [
						{
							label: "Delete route",
							run: () =>
								ec2Call(scope, "DeleteRoute", {
									RouteTableId: scope.id,
									DestinationCidrBlock: text(remove, "DestinationCidrBlock"),
								}),
						},
					]}
				>
					<p>Delete this route from {scope.id}.</p>
				</ActionDialog>
			)}
			{detach && (
				<ActionDialog
					title="Disassociate subnet"
					submit="Disassociate"
					onClose={() => setDetach(undefined)}
					onSaved={changed}
					plan={async () => [
						{
							label: "Disassociate subnet",
							run: () =>
								ec2Call(scope, "DisassociateRouteTable", {
									AssociationId: text(detach, "RouteTableAssociationId"),
								}),
						},
					]}
				>
					<p>{text(detach, "SubnetId")} will use the VPC's main route table.</p>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function RouteForm({
	scope,
	vpc,
	route,
	onClose,
	onSaved,
}: {
	scope: Ec2Scope;
	vpc: string;
	route: Ec2Row;
	onClose: () => void;
	onSaved: () => void;
}) {
	const existing = !!route.DestinationCidrBlock;
	const [cidr, setCidr] = useState(text(route, "DestinationCidrBlock")),
		[kind, setKind] = useState(
			route.NatGatewayId ? "nat" : route.InstanceId ? "instances" : "gateways",
		),
		[target, setTarget] = useState(
			text(route, "NatGatewayId") ||
				text(route, "InstanceId") ||
				text(route, "GatewayId"),
		);
	const choices = useChoices(scope, ["nat", "instances", "gateways"]);
	const section = kind as "nat" | "instances" | "gateways";
	return (
		<ActionDialog
			title={existing ? "Edit route" : "Add route"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!cidr.trim() || !target)
					throw new Error("Enter a destination CIDR and choose a target.");
				if (choices.loading || choices.error)
					throw new Error("Targets must finish loading.");
				return [
					{
						label: "Save route",
						run: () =>
							ec2Call(scope, existing ? "ReplaceRoute" : "CreateRoute", {
								RouteTableId: scope.id,
								DestinationCidrBlock: cidr.trim(),
								[kind === "nat"
									? "NatGatewayId"
									: kind === "instances"
										? "InstanceId"
										: "GatewayId"]: target,
							}),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<ChoiceStatus {...choices} />
				<Field
					label="Destination IPv4 CIDR"
					value={cidr}
					onChange={setCidr}
					disabled={existing}
				/>
				<Choice
					label="Target type"
					value={kind}
					onChange={(v) => {
						setKind(v);
						setTarget("");
					}}
					options={[
						{ label: "Internet gateway", value: "gateways" },
						{ label: "NAT gateway", value: "nat" },
						{ label: "Instance", value: "instances" },
					]}
				/>
				<Choice
					label="Target"
					value={target}
					onChange={setTarget}
					options={optionsFor(
						section,
						choices.items(section).filter((r) => parentVpc(r) === vpc),
					)}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function AssociationForm({
	scope,
	vpc,
	mode,
	onClose,
	onSaved,
}: {
	scope: Ec2Scope;
	vpc: string;
	mode: "route" | "acl";
	onClose: () => void;
	onSaved: () => void;
}) {
	const choices = useChoices(scope, ["subnets"]),
		[subnet, setSubnet] = useState("");
	return (
		<ActionDialog
			title={
				mode === "acl"
					? "Associate subnet with this network ACL"
					: "Associate subnet with this route table"
			}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!subnet || choices.loading || choices.error)
					throw new Error("Choose a subnet after resources finish loading.");
				// Re-read the current association immediately before saving, including associations on other tables/ACLs.
				const all = await ec2List(scope, mode === "acl" ? "acls" : "routes");
				const current = all
					.flatMap((r) => rows(r, "Associations"))
					.find((r) => text(r, "SubnetId") === subnet);
				return [
					{
						label: "Associate subnet",
						run: () =>
							mode === "acl"
								? ec2Call(scope, "ReplaceNetworkAclAssociation", {
										AssociationId: text(
											current ?? {},
											"NetworkAclAssociationId",
										),
										NetworkAclId: scope.id,
									})
								: current
									? ec2Call(scope, "ReplaceRouteTableAssociation", {
											AssociationId: text(current, "RouteTableAssociationId"),
											RouteTableId: scope.id,
										})
									: ec2Call(scope, "AssociateRouteTable", {
											SubnetId: subnet,
											RouteTableId: scope.id,
										}),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<ChoiceStatus {...choices} />
				<Choice
					label="Subnet"
					value={subnet}
					onChange={setSubnet}
					options={optionsFor(
						"subnets",
						choices.items("subnets").filter((r) => text(r, "VpcId") === vpc),
					)}
					description="This replaces the subnet's current explicit association."
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
