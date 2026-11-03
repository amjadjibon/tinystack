import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useEffect, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Buttons, Choice, Field } from "#/components/containers/Controls";
import { api } from "#/lib/api";
import { type CloudMapScope, cloudMapCall } from "#/lib/servicediscovery";
import {
	dnsRecords,
	type Namespace,
	namespaceInput,
	type Pair,
	pairMap,
	requiredName,
	type Service,
	tagList,
} from "#/lib/servicediscoveryHelpers";
import { usePoll } from "#/lib/usePoll";
import { Pairs } from "./Fields";
export type Edit = {
	kind: "namespace" | "service" | "instance";
	namespace?: Namespace;
	service?: Service;
	editing?: boolean;
};
export function ResourceDialog({
	scope,
	edit,
	onClose,
	onSaved,
	onOperation,
}: {
	scope: CloudMapScope;
	edit: Edit;
	onClose: () => void;
	onSaved: () => void;
	onOperation: (id: string) => void;
}) {
	const original =
		edit.kind === "namespace"
			? edit.namespace
			: edit.kind === "service"
				? edit.service
				: undefined;
	const [name, setName] = useState(edit.editing ? (original?.Name ?? "") : "");
	const [description, setDescription] = useState(
		edit.editing ? (original?.Description ?? "") : "",
	);
	const [type, setType] = useState("HTTP"),
		[vpc, setVpc] = useState("");
	const [dns, setDns] = useState(edit.service?.DnsConfig ? "DNS" : "HTTP");
	const [routing, setRouting] = useState("MULTIVALUE"),
		[health, setHealth] = useState("None");
	const [records, setRecords] = useState(
		edit.service?.DnsConfig?.DnsRecords.map((r) => ({
			Type: r.Type,
			TTL: String(r.TTL),
		})) ?? [{ Type: "A", TTL: "60" }],
	);
	const [attributes, setAttributes] = useState<Pair[]>([
		{ key: "AWS_INSTANCE_IPV4", value: "" },
		{ key: "AWS_INSTANCE_PORT", value: "" },
	]);
	const [tags, setTags] = useState<Pair[]>([]);
	const vpcs = usePoll(async () => {
		if (edit.kind !== "namespace" || edit.editing || type !== "DNS_PRIVATE")
			return [];
		const items: string[] = [],
			seen = new Set<string>();
		let token: string | undefined;
		do {
			const out = await api.action<{
				Vpcs?: { VpcId: string }[];
				NextToken?: string;
			}>(
				"ec2",
				"DescribeVpcs",
				scope.account,
				{ ...(token ? { NextToken: token } : {}) },
				scope.region,
			);
			items.push(...(out.Vpcs ?? []).map((v) => v.VpcId));
			token = out.NextToken;
			if (token && seen.has(token)) throw new Error("Repeated VPC page token.");
			if (token) seen.add(token);
		} while (token);
		return items;
	});
	useEffect(() => {
		if (type === "DNS_PRIVATE") vpcs.reload();
	}, [type, vpcs.reload]);
	return (
		<ActionDialog
			title={`${edit.editing ? "Edit" : "Create"} ${edit.kind}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				let operation: string, input: Record<string, unknown>;
				if (edit.kind === "namespace") {
					operation = edit.editing
						? "UpdateHttpNamespace"
						: ({
								HTTP: "CreateHttpNamespace",
								DNS_PRIVATE: "CreatePrivateDnsNamespace",
								DNS_PUBLIC: "CreatePublicDnsNamespace",
							}[type] ?? "");
					input = edit.editing
						? {
								Id: edit.namespace?.Id,
								Namespace: { Description: description },
							}
						: {
								...namespaceInput(name, description, type, vpc),
								Tags: tagList(tags),
							};
				} else if (edit.kind === "service") {
					const config =
						dns === "DNS" ? { DnsRecords: dnsRecords(records) } : undefined;
					operation = edit.editing ? "UpdateService" : "CreateService";
					input = edit.editing
						? {
								Id: edit.service?.Id,
								Service: {
									Description: description,
									...(config ? { DnsConfig: config } : {}),
								},
							}
						: {
								Name: requiredName(name),
								NamespaceId: edit.namespace?.Id,
								Description: description,
								Tags: tagList(tags),
								...(config
									? { DnsConfig: { ...config, RoutingPolicy: routing } }
									: {}),
								...(health === "Custom"
									? { HealthCheckCustomConfig: { FailureThreshold: 1 } }
									: {}),
							};
				} else {
					const attrs = pairMap(attributes);
					if (!Object.keys(attrs).length)
						throw new Error("At least one attribute is required.");
					operation = "RegisterInstance";
					input = {
						ServiceId: edit.service?.Id,
						InstanceId: requiredName(name),
						Attributes: attrs,
					};
				}
				if (!edit.editing) input.CreatorRequestId = crypto.randomUUID();
				return [
					{
						label: operation,
						run: async () => {
							const out = await cloudMapCall<{ OperationId?: string }>(
								scope,
								operation,
								input,
							);
							if (out.OperationId) onOperation(out.OperationId);
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!edit.editing && (
					<Field
						label={edit.kind === "instance" ? "Instance ID" : "Name"}
						value={name}
						onChange={setName}
					/>
				)}
				{edit.kind !== "instance" && (
					<Field
						label="Description"
						value={description}
						onChange={setDescription}
					/>
				)}
				{edit.kind === "namespace" && !edit.editing && (
					<SpaceBetween size="m">
						<Choice
							label="Namespace type"
							value={type}
							options={["HTTP", "DNS_PRIVATE", "DNS_PUBLIC"]}
							onChange={setType}
						/>
						{type === "DNS_PRIVATE" && (
							<SpaceBetween size="m">
								<Choice
									label="VPC"
									value={vpc}
									options={vpcs.data ?? []}
									onChange={setVpc}
								/>
								<Button onClick={vpcs.reload}>Refresh VPCs</Button>
								{!vpcs.loading && !vpcs.error && !vpcs.data?.length && (
									<Alert type="info">
										Create a VPC in the VPC console, then refresh this list.
									</Alert>
								)}
								{vpcs.error && <Alert type="error">{vpcs.error}</Alert>}
							</SpaceBetween>
						)}
					</SpaceBetween>
				)}
				{edit.kind === "service" && (
					<SpaceBetween size="m">
						{!edit.editing && edit.namespace?.Type !== "HTTP" && (
							<Choice
								label="Service discovery"
								value={dns}
								options={["HTTP", "DNS"]}
								onChange={setDns}
							/>
						)}
						{(edit.editing ? !!edit.service?.DnsConfig : dns === "DNS") && (
							<SpaceBetween size="m">
								{!edit.editing && (
									<Choice
										label="Routing policy"
										value={routing}
										options={["MULTIVALUE", "WEIGHTED"]}
										onChange={setRouting}
									/>
								)}
								{records.map((r, i) => (
									// biome-ignore lint/suspicious/noArrayIndexKey: DNS rows contain controlled inputs with no local state.
									<Buttons key={i}>
										{edit.editing ? (
											<strong>{r.Type}</strong>
										) : (
											<Choice
												label="Record type"
												value={r.Type}
												options={["A", "AAAA", "SRV", "CNAME"]}
												onChange={(Type) =>
													setRecords(
														records.map((x, j) =>
															i === j ? { ...x, Type } : x,
														),
													)
												}
											/>
										)}
										<Field
											label="TTL (seconds)"
											value={r.TTL}
											onChange={(TTL) =>
												setRecords(
													records.map((x, j) => (i === j ? { ...x, TTL } : x)),
												)
											}
										/>
										{!edit.editing && (
											<Button
												onClick={() =>
													setRecords(records.filter((_, j) => j !== i))
												}
											>
												Remove record
											</Button>
										)}
									</Buttons>
								))}
								{!edit.editing && (
									<Button
										onClick={() =>
											setRecords([...records, { Type: "A", TTL: "60" }])
										}
									>
										Add DNS record
									</Button>
								)}
							</SpaceBetween>
						)}
						{!edit.editing && (
							<Choice
								label="Health check"
								value={health}
								options={["None", "Custom"]}
								onChange={setHealth}
							/>
						)}
					</SpaceBetween>
				)}
				{edit.kind === "instance" && (
					<Pairs
						label="Attributes"
						rows={attributes}
						onChange={setAttributes}
					/>
				)}
				{!edit.editing && edit.kind !== "instance" && (
					<Pairs label="Tags" rows={tags} onChange={setTags} />
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
