import {
	Alert,
	Box,
	Container,
	CopyToClipboard,
	Header,
	SpaceBetween,
	StatusIndicator,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { Details } from "#/components/elbv2/Shared";
import type { Resource } from "#/lib/api";
import { unixTime } from "#/lib/format";
import { getDomainDetail, type OpenSearchScope } from "#/lib/opensearch";
import {
	configProcessing,
	connectCommand,
	creationDate,
	domainState,
	enabledText,
	hasPendingDomains,
	inventoryFailure,
	mastersText,
	optionRows,
	optionStateType,
	prettyPolicy,
	stateLabels,
	stateType,
	valueText,
} from "#/lib/opensearchHelpers";
import { usePoll } from "#/lib/usePoll";
import { DeleteDomainDialog, EditDomainDialog } from "./DomainDialogs";
import { DomainTags } from "./Shared";

type Dialog = "" | "edit" | "delete";

export function DomainDetails({
	scope,
	name,
	enabled,
	runtime,
	resources,
	onChanged,
	onBack,
}: {
	scope: OpenSearchScope;
	name: string;
	enabled: boolean;
	runtime?: string;
	resources: Resource[];
	onChanged: () => void;
	onBack: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>("");
	// A domain that is starting, being deleted or applying a change settles by itself, so the page reloads until it does.
	const [pending, setPending] = useState(false);
	const poll = usePoll(
		async () => {
			if (!enabled) return undefined;
			const detail = await getDomainDetail(scope, name);
			setPending(
				hasPendingDomains([detail.domain]) || configProcessing(detail.config),
			);
			return detail;
		},
		pending ? 3000 : 0,
	);
	const domain = poll.data?.domain;
	const config = poll.data?.config;
	const state = domain ? domainState(domain) : undefined;
	const command = connectCommand(runtime, domain?.Endpoint);
	const refresh = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!enabled && (
				<Alert type="info">
					Connect a console session to load domain details.
				</Alert>
			)}
			<Header
				variant="h1"
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action onClick={onBack}>Back to domains</Action>
						<Action
							disabled={!enabled || !domain || state === "deleting"}
							onClick={() => setDialog("edit")}
						>
							Edit
						</Action>
						<Action
							disabled={!enabled || !domain || state === "deleting"}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Action>
					</Buttons>
				}
			>
				{name}
			</Header>
			{state === "processing" && (
				<Alert type="info" header="Processing">
					The domain is starting or applying a change. This page reloads its
					status until it settles.
				</Alert>
			)}
			{state === "deleting" && (
				<Alert type="info" header="Deleting">
					The domain is being deleted. It disappears once its engine and data
					are removed.
				</Alert>
			)}
			{state === "failed" && (
				<Alert type="error" header="The domain failed">
					{inventoryFailure(resources, domain?.ARN) ||
						"The domain has no endpoint because its engine did not start."}{" "}
					It keeps its record until it is deleted.
				</Alert>
			)}
			{!!domain && !!state && (
				<Container header={<Header variant="h2">General</Header>}>
					<Details
						items={[
							["Domain name", domain.DomainName],
							[
								"Status",
								<StatusIndicator key="status" type={stateType(state)}>
									{stateLabels[state]}
								</StatusIndicator>,
							],
							["Engine version", domain.EngineVersion],
							[
								"Endpoint",
								<Copy key="endpoint" text={domain.Endpoint} label="Endpoint" />,
							],
							["ARN", <Copy key="arn" text={domain.ARN} label="ARN" />],
							["Domain ID", domain.DomainId],
							["Created", unixTime(creationDate(config))],
						]}
					/>
				</Container>
			)}
			{!!command && (
				<Container
					header={
						<Header
							variant="h2"
							description="The engine answers plain HTTP on this loopback port, and TinyStack does not authenticate it."
							actions={
								<CopyToClipboard
									variant="button"
									textToCopy={command}
									copyButtonText="Copy command"
									copySuccessText="Command copied"
									copyErrorText="Command failed to copy"
								/>
							}
						>
							Connect
						</Header>
					}
				>
					<CodeBlock text={command} />
				</Container>
			)}
			{domain && (
				<Container header={<Header variant="h2">Cluster</Header>}>
					<Details
						items={[
							["Instance type", valueText(domain.ClusterConfig?.InstanceType)],
							[
								"Instance count",
								valueText(domain.ClusterConfig?.InstanceCount),
							],
							["Dedicated master nodes", mastersText(domain.ClusterConfig)],
							[
								"Zone awareness",
								enabledText(domain.ClusterConfig?.ZoneAwarenessEnabled),
							],
						]}
					/>
				</Container>
			)}
			{domain && (
				<Container header={<Header variant="h2">Storage</Header>}>
					<Details
						items={[
							["EBS storage", enabledText(domain.EBSOptions?.EBSEnabled)],
							["Volume type", valueText(domain.EBSOptions?.VolumeType)],
							["Volume size", valueText(domain.EBSOptions?.VolumeSize, " GiB")],
							["Provisioned IOPS", valueText(domain.EBSOptions?.Iops)],
							[
								"Throughput",
								valueText(domain.EBSOptions?.Throughput, " MiB/s"),
							],
						]}
					/>
				</Container>
			)}
			{domain && (
				<Container header={<Header variant="h2">Security</Header>}>
					<Details
						items={[
							[
								"Encryption at rest",
								enabledText(domain.EncryptionAtRestOptions?.Enabled),
							],
							["KMS key", valueText(domain.EncryptionAtRestOptions?.KmsKeyId)],
							[
								"Node-to-node encryption",
								enabledText(domain.NodeToNodeEncryptionOptions?.Enabled),
							],
							[
								"Require HTTPS",
								enabledText(domain.DomainEndpointOptions?.EnforceHTTPS),
							],
							[
								"TLS security policy",
								valueText(domain.DomainEndpointOptions?.TLSSecurityPolicy),
							],
						]}
					/>
				</Container>
			)}
			{domain && (
				<Container header={<Header variant="h2">Access policy</Header>}>
					{domain.AccessPolicies ? (
						<CodeBlock text={prettyPolicy(domain.AccessPolicies)} />
					) : (
						<Box>No access policy</Box>
					)}
				</Container>
			)}
			{config && (
				<Container
					header={
						<Header
							variant="h2"
							description="A change to an option shows Processing until it has been applied."
						>
							Configuration status
						</Header>
					}
				>
					<Table
						variant="embedded"
						trackBy="id"
						items={optionRows(config)}
						columnDefinitions={[
							{ id: "option", header: "Option", cell: (o) => o.label },
							{
								id: "state",
								header: "State",
								cell: (o) => (
									<StatusIndicator type={optionStateType(o.status)}>
										{o.status?.PendingDeletion
											? "Pending deletion"
											: (o.status?.State ?? "-")}
									</StatusIndicator>
								),
							},
							{
								id: "version",
								header: "Version",
								cell: (o) => valueText(o.status?.UpdateVersion),
							},
							{
								id: "updated",
								header: "Last updated",
								cell: (o) => unixTime(o.status?.UpdateDate),
							},
						]}
					/>
				</Container>
			)}
			{domain && (
				<DomainTags scope={scope} arn={domain.ARN ?? ""} enabled={enabled} />
			)}
			{dialog === "edit" && domain && (
				<EditDomainDialog
					scope={scope}
					domain={domain}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						refresh();
					}}
				/>
			)}
			{dialog === "delete" && domain && (
				<DeleteDomainDialog
					scope={scope}
					domain={domain}
					docker={runtime === "docker"}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onChanged();
						onBack();
					}}
				/>
			)}
		</SpaceBetween>
	);
}
