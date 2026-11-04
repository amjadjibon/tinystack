import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { integer } from "#/lib/consoleActions";
import {
	type Ec2Row,
	type Ec2Scope,
	ec2Call,
	object,
	rows,
	text,
} from "#/lib/ec2";
import { usePoll } from "#/lib/usePoll";
import { ActionDialog } from "../ActionDialog";
import { Ec2Table } from "./Ec2Table";
import { Choice, ChoiceStatus, Field, optionsFor, useChoices } from "./Fields";

export function SecurityRules({
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
	const poll = usePoll(async () => {
		if (!enabled) return [];
		const result: Ec2Row[] = [];
		let token = "";
		const seen = new Set<string>();
		do {
			const out = await ec2Call(scope, "DescribeSecurityGroupRules", {
				Filters: [{ Name: "group-id", Values: [scope.id] }],
				MaxResults: 100,
				...(token ? { NextToken: token } : {}),
			});
			result.push(...rows(out, "SecurityGroupRules"));
			token = text(out, "NextToken");
			if (token && seen.has(token))
				throw new Error("The server repeated a pagination token.");
			seen.add(token);
		} while (token);
		return result;
	});
	const [add, setAdd] = useState<boolean>(),
		[remove, setRemove] = useState<Ec2Row>();
	const changed = () => {
		setAdd(undefined);
		setRemove(undefined);
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{[false, true].map((egress) => (
				<Ec2Table
					key={String(egress)}
					title={egress ? "Outbound rules" : "Inbound rules"}
					items={(poll.data ?? []).filter((r) => !!r.IsEgress === egress)}
					loading={poll.loading}
					trackBy="SecurityGroupRuleId"
					actions={
						<Button disabled={!enabled} onClick={() => setAdd(egress)}>
							Add {egress ? "outbound" : "inbound"} rule
						</Button>
					}
					columns={[
						{
							id: "id",
							header: "Rule ID",
							cell: (r) => text(r, "SecurityGroupRuleId"),
						},
						{
							id: "protocol",
							header: "Protocol",
							cell: (r) => text(r, "IpProtocol"),
						},
						{
							id: "ports",
							header: "Port range / ICMP",
							cell: (r) =>
								r.FromPort === undefined || text(r, "IpProtocol") === "-1"
									? "All"
									: `${r.FromPort} – ${r.ToPort}`,
						},
						{
							id: "source",
							header: egress ? "Destination" : "Source",
							cell: (r) =>
								text(r, "CidrIpv4") ||
								text(r, "CidrIpv6") ||
								text(r, "PrefixListId") ||
								text(object(r, "ReferencedGroupInfo"), "GroupId") ||
								"—",
						},
						{
							id: "description",
							header: "Description",
							cell: (r) => text(r, "Description") || "—",
						},
						{
							id: "actions",
							header: "Actions",
							cell: (r) => (
								<Button disabled={!enabled} onClick={() => setRemove(r)}>
									Remove
								</Button>
							),
						},
					]}
				/>
			))}
			{add !== undefined && (
				<RuleForm
					scope={scope}
					vpc={text(row, "VpcId")}
					egress={add}
					onClose={() => setAdd(undefined)}
					onSaved={changed}
				/>
			)}
			{remove && (
				<ActionDialog
					title="Remove security group rule"
					submit="Remove rule"
					confirmation={text(remove, "SecurityGroupRuleId")}
					onClose={() => setRemove(undefined)}
					onSaved={changed}
					plan={async () => [
						{
							label: "Remove rule",
							run: () =>
								ec2Call(
									scope,
									remove.IsEgress
										? "RevokeSecurityGroupEgress"
										: "RevokeSecurityGroupIngress",
									{
										GroupId: scope.id,
										SecurityGroupRuleIds: [text(remove, "SecurityGroupRuleId")],
									},
								),
						},
					]}
				>
					<p>
						Remove this {remove.IsEgress ? "outbound" : "inbound"} rule from{" "}
						{scope.id}.
					</p>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
function signed(value: string, label: string) {
	if (!/^-?\d+$/.test(value) || Number(value) < -1 || Number(value) > 255)
		throw new Error(`${label} must be an integer from -1 to 255.`);
	return Number(value);
}
function RuleForm({
	scope,
	vpc,
	egress,
	onClose,
	onSaved,
}: {
	scope: Ec2Scope;
	vpc: string;
	egress: boolean;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [protocol, setProtocol] = useState("tcp"),
		[from, setFrom] = useState("443"),
		[to, setTo] = useState("443"),
		[kind, setKind] = useState("ipv4"),
		[source, setSource] = useState(""),
		[description, setDescription] = useState("");
	const choices = useChoices(scope, ["groups"]);
	return (
		<ActionDialog
			title={`Add ${egress ? "outbound" : "inbound"} rule`}
			submit="Add rule"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!source.trim())
					throw new Error(`Enter a ${egress ? "destination" : "source"}.`);
				if (kind === "group" && (choices.loading || choices.error))
					throw new Error("Security groups must finish loading.");
				const permission: Ec2Row = { IpProtocol: protocol };
				if (protocol !== "-1") {
					permission.FromPort =
						protocol === "icmp" || protocol === "icmpv6"
							? signed(from, "ICMP type")
							: integer(from, "From port", 0, 65535);
					permission.ToPort =
						protocol === "icmp" || protocol === "icmpv6"
							? signed(to, "ICMP code")
							: integer(to, "To port", 0, 65535);
					if (
						(protocol === "tcp" || protocol === "udp") &&
						Number(permission.FromPort) > Number(permission.ToPort)
					)
						throw new Error("From port must be no greater than To port.");
				}
				const Description = description.trim() || undefined;
				if (kind === "ipv4")
					permission.IpRanges = [{ CidrIp: source.trim(), Description }];
				else if (kind === "ipv6")
					permission.Ipv6Ranges = [{ CidrIpv6: source.trim(), Description }];
				else if (kind === "group")
					permission.UserIdGroupPairs = [{ GroupId: source, Description }];
				else
					permission.PrefixListIds = [
						{ PrefixListId: source.trim(), Description },
					];
				return [
					{
						label: "Add rule",
						run: () =>
							ec2Call(
								scope,
								egress
									? "AuthorizeSecurityGroupEgress"
									: "AuthorizeSecurityGroupIngress",
								{ GroupId: scope.id, IpPermissions: [permission] },
							),
					},
				];
			}}
		>
			<SpaceBetween size="l">
				<ChoiceStatus {...choices} />
				<Choice
					label="Protocol"
					value={protocol}
					onChange={(v) => {
						setProtocol(v);
						if (v === "icmp" || v === "icmpv6") {
							setFrom("-1");
							setTo("-1");
						} else if (v === "tcp" || v === "udp") {
							if (Number(from) < 0) setFrom("443");
							if (Number(to) < 0) setTo("443");
						}
					}}
					options={[
						{ label: "TCP", value: "tcp" },
						{ label: "UDP", value: "udp" },
						{ label: "ICMP IPv4", value: "icmp" },
						{ label: "ICMP IPv6", value: "icmpv6" },
						{ label: "All traffic", value: "-1" },
					]}
				/>
				{protocol !== "-1" && (
					<SpaceBetween size="l">
						<Field
							label={protocol.startsWith("icmp") ? "ICMP type" : "From port"}
							value={from}
							onChange={setFrom}
						/>
						<Field
							label={protocol.startsWith("icmp") ? "ICMP code" : "To port"}
							value={to}
							onChange={setTo}
						/>
					</SpaceBetween>
				)}
				<Choice
					label={egress ? "Destination type" : "Source type"}
					value={kind}
					onChange={(v) => {
						setKind(v);
						setSource("");
					}}
					options={[
						{ label: "IPv4 CIDR", value: "ipv4" },
						{ label: "IPv6 CIDR", value: "ipv6" },
						{ label: "Security group", value: "group" },
						{ label: "Prefix list", value: "prefix" },
					]}
				/>
				{kind === "group" ? (
					<Choice
						label="Security group"
						value={source}
						onChange={setSource}
						options={optionsFor(
							"groups",
							choices.items("groups").filter((r) => text(r, "VpcId") === vpc),
						)}
					/>
				) : (
					<Field
						label={kind === "prefix" ? "Prefix list ID" : "CIDR block"}
						value={source}
						onChange={setSource}
						description={
							kind === "ipv4"
								? "For example, 10.0.0.0/16 or 192.0.2.10/32."
								: undefined
						}
					/>
				)}
				<Field
					label="Description"
					value={description}
					onChange={setDescription}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
