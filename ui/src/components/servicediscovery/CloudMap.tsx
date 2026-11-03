import { Alert, Button, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Buttons, Rows } from "#/components/containers/Controls";
import { unixTime } from "#/lib/format";
import {
	type CloudMapScope,
	cloudMapCall,
	cloudMapList,
} from "#/lib/servicediscovery";
import type {
	Instance,
	Namespace,
	Operation,
	Service,
} from "#/lib/servicediscoveryHelpers";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";
import { Discover } from "./Discover";
import { type Edit, ResourceDialog } from "./ResourceDialog";
export function CloudMap({
	scope,
	enabled,
	onChanged,
}: {
	scope: CloudMapScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const poll = usePoll(() =>
		enabled
			? cloudMapList<Namespace>(scope, "ListNamespaces", "Namespaces")
			: Promise.resolve([]),
	);
	const [namespace, setNamespace] = useState<Namespace>();
	const [operation, setOperation] = useState<string>();
	const [edit, setEdit] = useState<Edit>();
	const [deleting, setDeleting] = useState<Namespace>();
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="l">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Alert type="info">
				Cloud Map stores DNS configuration as records. Namespace and instance
				operations complete immediately; their status appears below.
			</Alert>
			<Rows
				title="Namespaces"
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="Create a namespace to begin."
				actions={
					<Buttons>
						<Button onClick={refresh}>Refresh</Button>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setEdit({ kind: "namespace" })}
						>
							Create namespace
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (n) => (
							<Button variant="link" onClick={() => setNamespace(n)}>
								{n.Name}
							</Button>
						),
					},
					{ id: "id", header: "ID", cell: (n) => n.Id },
					{ id: "type", header: "Type", cell: (n) => n.Type },
					{ id: "count", header: "Services", cell: (n) => n.ServiceCount ?? 0 },
					{
						id: "created",
						header: "Created",
						cell: (n) => unixTime(n.CreateDate),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (n) => (
							<Buttons>
								{n.Type === "HTTP" && (
									<Button
										disabled={!enabled}
										onClick={() =>
											setEdit({
												kind: "namespace",
												namespace: n,
												editing: true,
											})
										}
									>
										Edit description
									</Button>
								)}
								<Button disabled={!enabled} onClick={() => setDeleting(n)}>
									Delete
								</Button>
							</Buttons>
						),
					},
				]}
			/>
			{namespace && (
				<SpaceBetween size="l">
					<Button onClick={() => setNamespace(undefined)}>
						Close namespace
					</Button>
					<NamespaceServices
						key={namespace.Id}
						{...{
							scope,
							enabled,
							namespace:
								poll.data?.find((n) => n.Id === namespace.Id) ?? namespace,
							onChanged: refresh,
							onOperation: setOperation,
						}}
					/>
				</SpaceBetween>
			)}
			<Discover
				key={`${namespace?.Id ?? ""}`}
				scope={scope}
				enabled={enabled}
				namespaceName={namespace?.Name ?? ""}
			/>
			{operation && (
				<OperationStatus key={operation} scope={scope} id={operation} />
			)}
			{edit && (
				<ResourceDialog
					{...{ scope, edit }}
					onClose={() => setEdit(undefined)}
					onSaved={() => {
						setEdit(undefined);
						refresh();
					}}
					onOperation={setOperation}
				/>
			)}
			{deleting && (
				<DeleteDialog
					scope={scope}
					name={deleting.Name}
					operation="DeleteNamespace"
					input={{ Id: deleting.Id }}
					onClose={() => setDeleting(undefined)}
					onSaved={() => {
						if (namespace?.Id === deleting.Id) setNamespace(undefined);
						setDeleting(undefined);
						refresh();
					}}
					onOperation={setOperation}
				/>
			)}
		</SpaceBetween>
	);
}
function OperationStatus({ scope, id }: { scope: CloudMapScope; id: string }) {
	const poll = usePoll(() =>
		cloudMapCall<{ Operation: Operation }>(scope, "GetOperation", {
			OperationId: id,
		}),
	);
	return (
		<Alert
			type={
				poll.error || poll.data?.Operation.Status === "FAIL" ? "error" : "info"
			}
			header="Latest operation"
		>
			{poll.error ||
				`${id}: ${poll.data?.Operation.Status ?? "Loading"} (${poll.data?.Operation.Type ?? ""}) · ${unixTime(poll.data?.Operation.CreateDate)}`}
		</Alert>
	);
}
function DeleteDialog({
	scope,
	name,
	operation,
	input,
	onClose,
	onSaved,
	onOperation,
}: {
	scope: CloudMapScope;
	name: string;
	operation: string;
	input: Record<string, unknown>;
	onClose: () => void;
	onSaved: () => void;
	onOperation: (id: string) => void;
}) {
	return (
		<ActionDialog
			title={`Delete ${name}`}
			submit="Delete"
			confirmation={name}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
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
			]}
		>
			<Alert type="warning">
				This permanently removes {name}. Remove child resources first.
			</Alert>
		</ActionDialog>
	);
}
function Tags({
	scope,
	arn,
	enabled,
}: {
	scope: CloudMapScope;
	arn: string;
	enabled: boolean;
}) {
	const poll = usePoll(() =>
		cloudMapCall<{ Tags?: { Key: string; Value: string }[] }>(
			scope,
			"ListTagsForResource",
			{ ResourceARN: arn },
		),
	);
	return (
		<>
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<ConsoleTags
				tags={Object.fromEntries(
					(poll.data?.Tags ?? []).map((t) => [t.Key, t.Value]),
				)}
				enabled={enabled && !!poll.data && !poll.error}
				onChanged={poll.reload}
				plan={(next, removed) => [
					...(Object.keys(next).length
						? [
								{
									label: "TagResource",
									run: () =>
										cloudMapCall(scope, "TagResource", {
											ResourceARN: arn,
											Tags: tagList(next),
										}),
								},
							]
						: []),
					...(removed.length
						? [
								{
									label: "UntagResource",
									run: () =>
										cloudMapCall(scope, "UntagResource", {
											ResourceARN: arn,
											TagKeys: removed,
										}),
								},
							]
						: []),
				]}
			/>
		</>
	);
}
function NamespaceServices({
	scope,
	namespace,
	enabled,
	onChanged,
	onOperation,
}: {
	scope: CloudMapScope;
	namespace: Namespace;
	enabled: boolean;
	onChanged: () => void;
	onOperation: (id: string) => void;
}) {
	const poll = usePoll(() =>
		cloudMapList<Service>(scope, "ListServices", "Services", {
			Filters: [
				{ Name: "NAMESPACE_ID", Values: [namespace.Id], Condition: "EQ" },
			],
		}),
	);
	const [service, setService] = useState<Service>();
	const [edit, setEdit] = useState<Edit>();
	const [deleting, setDeleting] = useState<Service>();
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			<h2>{namespace.Name}</h2>
			<p>{namespace.Description || "-"}</p>
			<Tags scope={scope} arn={namespace.Arn} enabled={enabled} />
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Services"
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="Create a service in this namespace."
				actions={
					<Buttons>
						<Button onClick={refresh}>Refresh</Button>
						<Button
							disabled={!enabled}
							onClick={() => setEdit({ kind: "service", namespace })}
						>
							Create service
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (s) => (
							<Button variant="link" onClick={() => setService(s)}>
								{s.Name}
							</Button>
						),
					},
					{ id: "id", header: "ID", cell: (s) => s.Id },
					{
						id: "count",
						header: "Instances",
						cell: (s) => s.InstanceCount ?? 0,
					},
					{
						id: "dns",
						header: "DNS records",
						cell: (s) =>
							s.DnsConfig?.DnsRecords.map((r) => `${r.Type} (${r.TTL}s)`).join(
								", ",
							) || "-",
					},
					{
						id: "health",
						header: "Custom health",
						cell: (s) => (s.HealthCheckCustomConfig ? "Enabled" : "-"),
					},
					{
						id: "created",
						header: "Created",
						cell: (s) => unixTime(s.CreateDate),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (s) => (
							<Buttons>
								<Button
									disabled={!enabled}
									onClick={() =>
										setEdit({
											kind: "service",
											namespace,
											service: s,
											editing: true,
										})
									}
								>
									Edit
								</Button>
								<Button disabled={!enabled} onClick={() => setDeleting(s)}>
									Delete
								</Button>
							</Buttons>
						),
					},
				]}
			/>
			{service && (
				<SpaceBetween size="m">
					<Button onClick={() => setService(undefined)}>Close service</Button>
					<Instances
						key={service.Id}
						{...{
							scope,
							service: poll.data?.find((s) => s.Id === service.Id) ?? service,
							enabled,
							onChanged: refresh,
							onOperation,
						}}
					/>
				</SpaceBetween>
			)}
			{edit && (
				<ResourceDialog
					{...{ scope, edit, onOperation }}
					onClose={() => setEdit(undefined)}
					onSaved={() => {
						setEdit(undefined);
						refresh();
					}}
				/>
			)}
			{deleting && (
				<DeleteDialog
					scope={scope}
					name={deleting.Name}
					operation="DeleteService"
					input={{ Id: deleting.Id }}
					onOperation={onOperation}
					onClose={() => setDeleting(undefined)}
					onSaved={() => {
						if (service?.Id === deleting.Id) setService(undefined);
						setDeleting(undefined);
						refresh();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
function Instances({
	scope,
	service,
	enabled,
	onChanged,
	onOperation,
}: {
	scope: CloudMapScope;
	service: Service;
	enabled: boolean;
	onChanged: () => void;
	onOperation: (id: string) => void;
}) {
	const poll = usePoll(async () => {
		const instances = await cloudMapList<Instance>(
			scope,
			"ListInstances",
			"Instances",
			{ ServiceId: service.Id },
		);
		const health: Record<string, string> = {},
			seen = new Set<string>();
		let token: string | undefined;
		do {
			const out = await cloudMapCall<{
				Status: Record<string, string>;
				NextToken?: string;
			}>(scope, "GetInstancesHealthStatus", {
				ServiceId: service.Id,
				MaxResults: 100,
				...(token ? { NextToken: token } : {}),
			});
			Object.assign(health, out.Status);
			token = out.NextToken;
			if (token && seen.has(token))
				throw new Error("Repeated health status page token.");
			if (token) seen.add(token);
		} while (token);
		return instances.map((i) => ({ ...i, health: health[i.Id] ?? "-" }));
	});
	const [creating, setCreating] = useState(false),
		[deleting, setDeleting] = useState<Instance>();
	const [healthAction, setHealthAction] = useState<{
		id: string;
		status: string;
	}>();
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			<h3>{service.Name}</h3>
			<p>{service.Description || "-"}</p>
			<Tags scope={scope} arn={service.Arn} enabled={enabled} />
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Instances"
				items={poll.data ?? []}
				loading={poll.loading}
				emptyText="Register an instance."
				actions={
					<Buttons>
						<Button onClick={refresh}>Refresh</Button>
						<Button disabled={!enabled} onClick={() => setCreating(true)}>
							Register instance
						</Button>
					</Buttons>
				}
				columns={[
					{ id: "id", header: "Instance ID", cell: (i) => i.Id },
					{
						id: "attributes",
						header: "Attributes",
						cell: (i) => JSON.stringify(i.Attributes),
					},
					{ id: "health", header: "Health", cell: (i) => i.health },
					{
						id: "actions",
						header: "Actions",
						cell: (i) => (
							<Buttons>
								<Button disabled={!enabled} onClick={() => setDeleting(i)}>
									Deregister
								</Button>
								{service.HealthCheckCustomConfig &&
									["HEALTHY", "UNHEALTHY"].map((status) => (
										<Button
											key={status}
											disabled={!enabled}
											onClick={() => setHealthAction({ id: i.Id, status })}
										>
											{status}
										</Button>
									))}
							</Buttons>
						),
					},
				]}
			/>
			{creating && (
				<ResourceDialog
					scope={scope}
					edit={{ kind: "instance", service }}
					onOperation={onOperation}
					onClose={() => setCreating(false)}
					onSaved={() => {
						setCreating(false);
						refresh();
					}}
				/>
			)}
			{deleting && (
				<DeleteDialog
					scope={scope}
					name={deleting.Id}
					operation="DeregisterInstance"
					input={{ ServiceId: service.Id, InstanceId: deleting.Id }}
					onOperation={onOperation}
					onClose={() => setDeleting(undefined)}
					onSaved={() => {
						setDeleting(undefined);
						refresh();
					}}
				/>
			)}
			{healthAction && (
				<ActionDialog
					title={`Set ${healthAction.id} ${healthAction.status}`}
					onClose={() => setHealthAction(undefined)}
					onSaved={() => {
						setHealthAction(undefined);
						refresh();
					}}
					plan={async () => [
						{
							label: "Update custom health",
							run: () =>
								cloudMapCall(scope, "UpdateInstanceCustomHealthStatus", {
									ServiceId: service.Id,
									InstanceId: healthAction.id,
									Status: healthAction.status,
								}),
						},
					]}
				>
					<p>Update the custom health status immediately.</p>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
