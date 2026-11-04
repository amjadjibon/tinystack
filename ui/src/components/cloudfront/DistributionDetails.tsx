import {
	Alert,
	Container,
	Header,
	SpaceBetween,
	StatusIndicator,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons } from "#/components/containers/Controls";
import { Details } from "#/components/elbv2/Shared";
import {
	type CloudFrontScope,
	getDistribution,
	type OacSummary,
} from "#/lib/cloudfront";
import {
	behaviorRows,
	distributionStatusType,
	originRows,
} from "#/lib/cloudfrontHelpers";
import { isoTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";
import {
	DeleteDistributionDialog,
	EditDistributionDialog,
	ToggleDistributionDialog,
} from "./DistributionDialogs";
import { Invalidations } from "./Invalidations";
import { Copy, ResourceTags } from "./Shared";

type Dialog = "" | "edit" | "toggle" | "delete";

export function DistributionDetails({
	scope,
	id,
	enabled,
	oacs,
	onChanged,
	onBack,
}: {
	scope: CloudFrontScope;
	id: string;
	enabled: boolean;
	oacs: OacSummary[];
	onChanged: () => void;
	onBack: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>("");
	const poll = usePoll(async () =>
		enabled ? await getDistribution(scope, id) : undefined,
	);
	const distribution = poll.data?.Distribution;
	const config = distribution?.DistributionConfig;
	const on = !!config?.Enabled;
	const changed = () => {
		setDialog("");
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!enabled && (
				<Alert type="info">
					Connect a console session to load distribution details.
				</Alert>
			)}
			<Header
				variant="h1"
				description={config?.Comment}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action onClick={onBack}>Back to distributions</Action>
						<Action
							disabled={!enabled || !config}
							onClick={() => setDialog("edit")}
						>
							Edit
						</Action>
						<Action
							disabled={!enabled || !config}
							onClick={() => setDialog("toggle")}
						>
							{on ? "Disable" : "Enable"}
						</Action>
						<Action
							disabled={!enabled || !config}
							onClick={() => setDialog("delete")}
						>
							Delete
						</Action>
					</Buttons>
				}
			>
				{id}
			</Header>
			{distribution && (
				<Container header={<Header variant="h2">General</Header>}>
					<Details
						items={[
							["Distribution ID", <Copy key="id" text={id} label="ID" />],
							["ARN", <Copy key="arn" text={distribution.ARN} label="ARN" />],
							[
								"Domain name",
								<Copy
									key="domain"
									text={distribution.DomainName}
									label="Domain name"
								/>,
							],
							[
								"Status",
								<StatusIndicator
									key="status"
									type={distributionStatusType(distribution.Status)}
								>
									{distribution.Status}
								</StatusIndicator>,
							],
							["State", on ? "Enabled" : "Disabled"],
							["Last modified", isoTime(distribution.LastModifiedTime)],
							["ETag", <Copy key="etag" text={poll.data?.ETag} label="ETag" />],
							["Alternate domain names", config?.Aliases?.Items?.join(", ")],
							["Default root object", config?.DefaultRootObject],
							[
								"Invalidations in progress",
								distribution.InProgressInvalidationBatches?.toString(),
							],
						]}
					/>
				</Container>
			)}
			{config && (
				<Table
					variant="container"
					header={<Header variant="h2">Origins</Header>}
					items={originRows(config)}
					trackBy="id"
					empty="No origins"
					columnDefinitions={[
						{ id: "id", header: "Origin ID", cell: (o) => o.id },
						{ id: "domain", header: "Domain name", cell: (o) => o.domain },
						{ id: "kind", header: "Type", cell: (o) => o.kind },
						{ id: "path", header: "Path", cell: (o) => o.path },
						{
							id: "oac",
							header: "Origin access control",
							cell: (o) => o.accessControl,
						},
					]}
				/>
			)}
			{config && (
				<Table
					variant="container"
					header={<Header variant="h2">Behaviors</Header>}
					items={behaviorRows(config)}
					trackBy="path"
					empty="No behaviors"
					columnDefinitions={[
						{ id: "path", header: "Path pattern", cell: (b) => b.path },
						{ id: "target", header: "Target origin", cell: (b) => b.target },
						{
							id: "viewer",
							header: "Viewer protocol policy",
							cell: (b) => b.viewerProtocol,
						},
						{
							id: "methods",
							header: "Allowed methods",
							cell: (b) => b.methods,
						},
						{ id: "compress", header: "Compress", cell: (b) => b.compress },
						{
							id: "cache",
							header: "Cache policy",
							cell: (b) => b.cachePolicy,
						},
					]}
				/>
			)}
			{distribution && (
				<Invalidations scope={scope} distribution={id} enabled={enabled} />
			)}
			{distribution && (
				<ResourceTags scope={scope} arn={distribution.ARN} enabled={enabled} />
			)}
			{dialog === "edit" && config && (
				<EditDistributionDialog
					scope={scope}
					id={id}
					config={config}
					oacs={oacs}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "toggle" && (
				<ToggleDistributionDialog
					scope={scope}
					id={id}
					enable={!on}
					onClose={() => setDialog("")}
					onSaved={changed}
				/>
			)}
			{dialog === "delete" && (
				<DeleteDistributionDialog
					scope={scope}
					id={id}
					enabled={on}
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
