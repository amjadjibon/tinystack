import {
	Alert,
	Box,
	Button,
	FormField,
	Modal,
	SpaceBetween,
	StatusIndicator,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Details } from "#/components/elbv2/Shared";
import {
	type CloudFrontScope,
	cloudfrontCall,
	type Invalidation,
	listInvalidations,
} from "#/lib/cloudfront";
import {
	callerReference,
	createInvalidationInput,
} from "#/lib/cloudfrontHelpers";
import { isoTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";

function InvalidationStatus({ status }: { status: string }) {
	return (
		<StatusIndicator type={status === "Completed" ? "success" : "in-progress"}>
			{status}
		</StatusIndicator>
	);
}

function ViewInvalidation({
	scope,
	distribution,
	id,
	onClose,
}: {
	scope: CloudFrontScope;
	distribution: string;
	id: string;
	onClose: () => void;
}) {
	const poll = usePoll(
		() =>
			cloudfrontCall<{ Invalidation: Invalidation }>(scope, "GetInvalidation", {
				DistributionId: distribution,
				Id: id,
			}),
		0,
	);
	const invalidation = poll.data?.Invalidation;
	return (
		<Modal
			visible
			size="large"
			header={`Invalidation ${id}`}
			closeAriaLabel="Close dialog"
			onDismiss={onClose}
			footer={
				<Box float="right">
					<Button variant="primary" onClick={onClose}>
						Close
					</Button>
				</Box>
			}
		>
			<SpaceBetween size="m">
				{poll.error && <Alert type="error">{poll.error}</Alert>}
				<Details
					items={[
						[
							"Status",
							invalidation && (
								<InvalidationStatus status={invalidation.Status} />
							),
						],
						["Created", isoTime(invalidation?.CreateTime)],
						[
							"Caller reference",
							invalidation?.InvalidationBatch?.CallerReference,
						],
					]}
				/>
				<CodeBlock
					text={(invalidation?.InvalidationBatch?.Paths?.Items ?? []).join(
						"\n",
					)}
				/>
			</SpaceBetween>
		</Modal>
	);
}

export function Invalidations({
	scope,
	distribution,
	enabled,
}: {
	scope: CloudFrontScope;
	distribution: string;
	enabled: boolean;
}) {
	const [creating, setCreating] = useState(false);
	const [viewing, setViewing] = useState("");
	const [paths, setPaths] = useState("");
	const poll = usePoll(() =>
		enabled ? listInvalidations(scope, distribution) : Promise.resolve([]),
	);
	// The newest invalidation comes first, as in CloudFront.
	const items = [...(poll.data ?? [])].sort((a, b) =>
		(b.CreateTime ?? "").localeCompare(a.CreateTime ?? ""),
	);
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Invalidations"
				emptyText="Choose Create invalidation to clear cached paths."
				items={items}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => setCreating(true)}>
							Create invalidation
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "id",
						header: "ID",
						cell: (i) => (
							<Action onClick={() => setViewing(i.Id)}>{i.Id}</Action>
						),
					},
					{
						id: "status",
						header: "Status",
						cell: (i) => <InvalidationStatus status={i.Status} />,
					},
					{
						id: "created",
						header: "Created",
						cell: (i) => isoTime(i.CreateTime),
					},
				]}
			/>
			{creating && (
				<ActionDialog
					title="Create invalidation"
					submit="Create invalidation"
					onClose={() => setCreating(false)}
					onSaved={() => {
						setCreating(false);
						setPaths("");
						poll.reload();
					}}
					plan={async () => {
						const input = createInvalidationInput(
							distribution,
							paths,
							callerReference("console"),
						);
						return [
							{
								label: "CreateInvalidation",
								run: () => cloudfrontCall(scope, "CreateInvalidation", input),
							},
						];
					}}
				>
					<FormField
						label="Paths"
						description="One path per line. Each starts with a slash, and an asterisk can end a path, for example /images/*."
					>
						<Textarea
							ariaLabel="Invalidation paths"
							rows={6}
							placeholder="/index.html"
							value={paths}
							onChange={({ detail }) => setPaths(detail.value)}
						/>
					</FormField>
				</ActionDialog>
			)}
			{!!viewing && (
				<ViewInvalidation
					scope={scope}
					distribution={distribution}
					id={viewing}
					onClose={() => setViewing("")}
				/>
			)}
		</SpaceBetween>
	);
}
