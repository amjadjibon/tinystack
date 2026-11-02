import {
	Alert,
	Link,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import {
	type CloudFrontScope,
	cloudfrontHref,
	type DistributionSummary,
	listDistributions,
	type OacSummary,
} from "#/lib/cloudfront";
import { distributionStatusType, originsText } from "#/lib/cloudfrontHelpers";
import { isoTime } from "#/lib/format";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CreateDistributionDialog } from "./DistributionDialogs";
import { Copy } from "./Shared";

export function Distributions({
	scope,
	enabled,
	oacs,
	onCreated,
}: {
	scope: CloudFrontScope;
	enabled: boolean;
	oacs: OacSummary[];
	onCreated: (id: string) => void;
}) {
	const follow = useFollow();
	const [creating, setCreating] = useState(false);
	const poll = usePoll(() =>
		enabled ? listDistributions(scope) : Promise.resolve([]),
	);
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<DistributionSummary>
				title="Distributions"
				emptyText="Choose Create distribution to add one."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => setCreating(true)}>
							Create distribution
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "id",
						header: "ID",
						cell: (d) => (
							<Link
								href={cloudfrontHref({ ...scope, distribution: d.Id })}
								onFollow={follow}
							>
								{d.Id}
							</Link>
						),
					},
					{
						id: "domain",
						header: "Domain name",
						cell: (d) => <Copy text={d.DomainName} label="Domain name" />,
					},
					{
						id: "status",
						header: "Status",
						cell: (d) => (
							<StatusIndicator type={distributionStatusType(d.Status)}>
								{d.Status}
							</StatusIndicator>
						),
					},
					{
						id: "enabled",
						header: "Enabled",
						cell: (d) => (d.Enabled ? "Enabled" : "Disabled"),
					},
					{ id: "origins", header: "Origins", cell: (d) => originsText(d) },
					{
						id: "modified",
						header: "Last modified",
						cell: (d) => isoTime(d.LastModifiedTime),
					},
				]}
			/>
			{creating && (
				<CreateDistributionDialog
					scope={scope}
					oacs={oacs}
					onClose={() => setCreating(false)}
					onSaved={(id) => {
						setCreating(false);
						onCreated(id);
					}}
				/>
			)}
		</SpaceBetween>
	);
}
