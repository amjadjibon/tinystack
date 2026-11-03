import { Button, Link, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { integer } from "#/lib/consoleActions";
import {
	type Ec2Row,
	type Ec2Scope,
	ec2Call,
	ec2Href,
	object,
	rows,
	text,
} from "#/lib/ec2";
import { useFollow } from "#/lib/useFollow";
import { ActionDialog } from "../ActionDialog";
import { Ec2Table } from "./Ec2Table";
import { Choice, Field } from "./Fields";
import { AssociationForm } from "./Routes";

export function NetworkAcl({
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
		[associate, setAssociate] = useState(false);
	const follow = useFollow(),
		changed = () => {
			setEdit(undefined);
			setRemove(undefined);
			setAssociate(false);
			onChanged();
		};
	return (
		<SpaceBetween size="l">
			{[false, true].map((egress) => (
				<Ec2Table
					key={String(egress)}
					title={egress ? "Outbound rules" : "Inbound rules"}
					items={rows(row, "Entries")
						.filter((r) => !!r.Egress === egress)
						.sort((a, b) => Number(a.RuleNumber) - Number(b.RuleNumber))}
					trackBy="RuleNumber"
					actions={
						<Button
							disabled={!enabled}
							onClick={() => setEdit({ Egress: egress })}
						>
							Add rule
						</Button>
					}
					columns={[
						{
							id: "number",
							header: "Rule number",
							cell: (r) =>
								text(r, "RuleNumber") === "32767" ? "*" : text(r, "RuleNumber"),
						},
						{
							id: "protocol",
							header: "Protocol",
							cell: (r) =>
								text(r, "Protocol") === "-1" ? "All" : text(r, "Protocol"),
						},
						{
							id: "range",
							header: "Port range / ICMP",
							cell: (r) =>
								r.PortRange
									? `${object(r, "PortRange").From} – ${object(r, "PortRange").To}`
									: r.IcmpTypeCode
										? `${object(r, "IcmpTypeCode").Type} / ${object(r, "IcmpTypeCode").Code}`
										: "All",
						},
						{
							id: "cidr",
							header: egress ? "Destination" : "Source",
							cell: (r) => text(r, "CidrBlock"),
						},
						{
							id: "action",
							header: "Allow / deny",
							cell: (r) => text(r, "RuleAction"),
						},
						{
							id: "edit",
							header: "Actions",
							cell: (r) =>
								Number(r.RuleNumber) === 32767 ? (
									"Default rule"
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
			))}
			<Ec2Table
				title="Subnet associations"
				description="To remove an association, open another ACL (including the default ACL) and associate the subnet there."
				trackBy="NetworkAclAssociationId"
				items={rows(row, "Associations")}
				actions={
					<Button disabled={!enabled} onClick={() => setAssociate(true)}>
						Associate subnet
					</Button>
				}
				columns={[
					{
						id: "id",
						header: "Association ID",
						cell: (r) => text(r, "NetworkAclAssociationId"),
					},
					{
						id: "subnet",
						header: "Subnet",
						cell: (r) => (
							<Link
								onFollow={follow}
								href={ec2Href({
									...scope,
									section: "subnets",
									id: text(r, "SubnetId"),
								})}
							>
								{text(r, "SubnetId")}
							</Link>
						),
					},
				]}
			/>
			{edit && (
				<AclForm
					scope={scope}
					entry={edit}
					onClose={() => setEdit(undefined)}
					onSaved={changed}
				/>
			)}
			{associate && (
				<AssociationForm
					scope={scope}
					vpc={text(row, "VpcId")}
					mode="acl"
					onClose={() => setAssociate(false)}
					onSaved={changed}
				/>
			)}
			{remove && (
				<ActionDialog
					title="Delete network ACL rule"
					submit="Delete rule"
					confirmation={text(remove, "RuleNumber")}
					onClose={() => setRemove(undefined)}
					onSaved={changed}
					plan={async () => [
						{
							label: "Delete rule",
							run: () =>
								ec2Call(scope, "DeleteNetworkAclEntry", {
									NetworkAclId: scope.id,
									RuleNumber: remove.RuleNumber,
									Egress: !!remove.Egress,
								}),
						},
					]}
				>
					<p>
						Delete this {remove.Egress ? "outbound" : "inbound"} rule from{" "}
						{scope.id}.
					</p>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function AclForm({
	scope,
	entry,
	onClose,
	onSaved,
}: {
	scope: Ec2Scope;
	entry: Ec2Row;
	onClose: () => void;
	onSaved: () => void;
}) {
	const existing = entry.RuleNumber !== undefined;
	const [number, setNumber] = useState(
			existing ? text(entry, "RuleNumber") : "100",
		),
		[protocol, setProtocol] = useState(text(entry, "Protocol") || "6"),
		[action, setAction] = useState(text(entry, "RuleAction") || "allow"),
		[cidr, setCidr] = useState(text(entry, "CidrBlock")),
		[from, setFrom] = useState(
			String(
				object(entry, "PortRange").From ??
					object(entry, "IcmpTypeCode").Type ??
					443,
			),
		),
		[to, setTo] = useState(
			String(
				object(entry, "PortRange").To ??
					object(entry, "IcmpTypeCode").Code ??
					443,
			),
		);
	const signed = (v: string, label: string) => {
		if (!/^-?\d+$/.test(v) || Number(v) < -1 || Number(v) > 255)
			throw new Error(`${label} must be an integer from -1 to 255.`);
		return Number(v);
	};
	return (
		<ActionDialog
			title={existing ? "Edit network ACL rule" : "Add network ACL rule"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!cidr.trim()) throw new Error("Enter an IPv4 CIDR block.");
				const input: Ec2Row = {
					NetworkAclId: scope.id,
					RuleNumber: integer(number, "Rule number", 1, 32766),
					Egress: !!entry.Egress,
					Protocol: protocol,
					RuleAction: action,
					CidrBlock: cidr.trim(),
				};
				if (protocol === "6" || protocol === "17") {
					const From = integer(from, "From port", 0, 65535),
						To = integer(to, "To port", 0, 65535);
					if (From > To)
						throw new Error("From port must be no greater than To port.");
					input.PortRange = { From, To };
				}
				if (protocol === "1")
					input.IcmpTypeCode = {
						Type: signed(from, "ICMP type"),
						Code: signed(to, "ICMP code"),
					};
				return [
					{
						label: "Save rule",
						run: () =>
							ec2Call(
								scope,
								existing ? "ReplaceNetworkAclEntry" : "CreateNetworkAclEntry",
								input,
							),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<Field
					label="Rule number"
					value={number}
					onChange={setNumber}
					disabled={existing}
					description="Rules are evaluated from the lowest number first. Use 1–32766."
				/>
				<Choice
					label="Protocol"
					value={protocol}
					onChange={(v) => {
						setProtocol(v);
						if (v === "1") {
							setFrom("-1");
							setTo("-1");
						} else if (v === "6" || v === "17") {
							if (Number(from) < 0) setFrom("443");
							if (Number(to) < 0) setTo("443");
						}
					}}
					options={[
						{ label: "TCP", value: "6" },
						{ label: "UDP", value: "17" },
						{ label: "ICMP IPv4", value: "1" },
						{ label: "All traffic", value: "-1" },
					]}
				/>
				{protocol !== "-1" && (
					<SpaceBetween size="l">
						<Field
							label={protocol === "1" ? "ICMP type" : "From port"}
							value={from}
							onChange={setFrom}
						/>
						<Field
							label={protocol === "1" ? "ICMP code" : "To port"}
							value={to}
							onChange={setTo}
						/>
					</SpaceBetween>
				)}
				<Field
					label={entry.Egress ? "Destination IPv4 CIDR" : "Source IPv4 CIDR"}
					value={cidr}
					onChange={setCidr}
				/>
				<Choice
					label="Rule action"
					value={action}
					onChange={setAction}
					options={[
						{ label: "Allow", value: "allow" },
						{ label: "Deny", value: "deny" },
					]}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
