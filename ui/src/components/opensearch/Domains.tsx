import {
	Alert,
	Button,
	Link,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { unixTime } from "#/lib/format";
import {
	listDomains,
	type OpenSearchScope,
	opensearchHref,
} from "#/lib/opensearch";
import {
	capacityText,
	type DomainRow,
	domainState,
	hasPendingDomains,
	stateLabels,
	stateType,
} from "#/lib/opensearchHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CreateDomainDialog } from "./DomainDialogs";

export function Domains({
	scope,
	enabled,
	onCreated,
}: {
	scope: OpenSearchScope;
	enabled: boolean;
	onCreated: (name: string) => void;
}) {
	const follow = useFollow();
	const [creating, setCreating] = useState(false);
	// A domain that is starting or being deleted settles by itself, so the list reloads until none is.
	const [pending, setPending] = useState(false);
	const poll = usePoll(
		async () => {
			const domains = enabled ? await listDomains(scope) : [];
			setPending(hasPendingDomains(domains));
			return domains;
		},
		pending ? 3000 : 0,
	);
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<DomainRow>
				title="Domains"
				emptyText="Choose Create domain to add one."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setCreating(true)}
						>
							Create domain
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Domain name",
						cell: (d) => (
							<Link
								href={opensearchHref({ ...scope, domain: d.DomainName })}
								onFollow={follow}
							>
								{d.DomainName}
							</Link>
						),
					},
					{
						id: "status",
						header: "Status",
						cell: (d) => {
							const state = domainState(d);
							return (
								<StatusIndicator type={stateType(state)}>
									{stateLabels[state]}
								</StatusIndicator>
							);
						},
					},
					{
						id: "version",
						header: "Engine version",
						cell: (d) => d.EngineVersion || "-",
					},
					{
						id: "capacity",
						header: "Instances",
						cell: (d) => capacityText(d.ClusterConfig),
					},
					{
						id: "created",
						header: "Created",
						cell: (d) => unixTime(d.CreationDate),
					},
					{
						id: "endpoint",
						header: "Endpoint",
						cell: (d) => <Copy text={d.Endpoint} label="Endpoint" />,
					},
				]}
			/>
			{creating && (
				<CreateDomainDialog
					scope={scope}
					onClose={() => setCreating(false)}
					onSaved={(name) => {
						setCreating(false);
						onCreated(name);
					}}
				/>
			)}
		</SpaceBetween>
	);
}
