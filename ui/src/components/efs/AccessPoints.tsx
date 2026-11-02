import {
	Alert,
	Button,
	Container,
	Header,
	Link,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Details } from "#/components/elbv2/Shared";
import { type EFSScope, efsCall, efsHref, listAccessPoints } from "#/lib/efs";
import {
	type AccessPoint,
	creationInfoText,
	lifeCycleStatusType,
	posixUserText,
} from "#/lib/efsHelpers";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
import { CreateAccessPointDialog } from "./AccessPointDialog";
import { Copy, ResourceTags } from "./Shared";

type Dialog =
	| { kind: "" }
	| { kind: "create" }
	| { kind: "delete"; point: AccessPoint };

// AccessPoints lists the access points of one file system, or of all of them when
// none is given, and shows the details and tags of the one that was chosen.
export function AccessPoints({
	scope,
	enabled,
	fileSystem,
	onChanged,
}: {
	scope: EFSScope;
	enabled: boolean;
	fileSystem?: string;
	onChanged: () => void;
}) {
	const follow = useFollow();
	const [dialog, setDialog] = useState<Dialog>({ kind: "" });
	const [selected, setSelected] = useState("");
	const poll = usePoll(() =>
		enabled ? listAccessPoints(scope, fileSystem) : Promise.resolve([]),
	);
	const points = poll.data ?? [];
	const detail = points.find((p) => p.AccessPointId === selected);
	const close = () => setDialog({ kind: "" });
	const saved = () => {
		close();
		poll.reload();
		onChanged();
	};
	const fileSystemLink = (id?: string) =>
		id && !fileSystem ? (
			<Link href={efsHref({ ...scope, filesystem: id })} onFollow={follow}>
				{id}
			</Link>
		) : (
			id
		);
	return (
		<SpaceBetween size="l">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<AccessPoint>
				title="Access points"
				emptyText="Choose Create access point to add one."
				items={points}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog({ kind: "create" })}
						>
							Create access point
						</Button>
					</Buttons>
				}
				columns={[
					{
						id: "id",
						header: "Access point ID",
						cell: (p) => (
							<Action onClick={() => setSelected(p.AccessPointId)}>
								{p.AccessPointId}
							</Action>
						),
					},
					{ id: "name", header: "Name", cell: (p) => p.Name || "-" },
					...(fileSystem
						? []
						: [
								{
									id: "filesystem",
									header: "File system",
									cell: (p: AccessPoint) =>
										fileSystemLink(p.FileSystemId) ?? "-",
								},
							]),
					{
						id: "state",
						header: "State",
						cell: (p) => (
							<StatusIndicator type={lifeCycleStatusType(p.LifeCycleState)}>
								{p.LifeCycleState ?? "-"}
							</StatusIndicator>
						),
					},
					{
						id: "posix",
						header: "POSIX user (UID:GID)",
						cell: (p) => posixUserText(p.PosixUser),
					},
					{
						id: "root",
						header: "Root directory",
						cell: (p) => p.RootDirectory?.Path ?? "/",
					},
					{
						id: "actions",
						header: "Actions",
						cell: (p) => (
							<Action
								disabled={!enabled}
								onClick={() => setDialog({ kind: "delete", point: p })}
							>
								Delete
							</Action>
						),
					},
				]}
			/>
			{detail && (
				<SpaceBetween size="l" key={detail.AccessPointId}>
					<Container
						header={
							<Header
								variant="h2"
								actions={<Action onClick={() => setSelected("")}>Close</Action>}
							>
								{detail.Name || detail.AccessPointId}
							</Header>
						}
					>
						<Details
							items={[
								[
									"Access point ID",
									<Copy
										key="id"
										text={detail.AccessPointId}
										label="Access point ID"
									/>,
								],
								[
									"ARN",
									<Copy key="arn" text={detail.AccessPointArn} label="ARN" />,
								],
								["Name", detail.Name],
								["File system", fileSystemLink(detail.FileSystemId)],
								[
									"State",
									<StatusIndicator
										key="state"
										type={lifeCycleStatusType(detail.LifeCycleState)}
									>
										{detail.LifeCycleState ?? "-"}
									</StatusIndicator>,
								],
								["Owner", detail.OwnerId],
								["POSIX user (UID:GID)", posixUserText(detail.PosixUser)],
								["Root directory", detail.RootDirectory?.Path ?? "/"],
								[
									"Root directory creation info",
									creationInfoText(detail.RootDirectory),
								],
								["Client token", detail.ClientToken],
							]}
						/>
					</Container>
					<ResourceTags
						scope={scope}
						id={detail.AccessPointId}
						tags={detail.Tags}
						enabled={enabled}
						onChanged={poll.reload}
					/>
				</SpaceBetween>
			)}
			{dialog.kind === "create" && (
				<CreateAccessPointDialog
					scope={scope}
					fileSystem={fileSystem}
					onClose={close}
					onSaved={(id) => {
						setSelected(id);
						saved();
					}}
				/>
			)}
			{dialog.kind === "delete" && (
				<ActionDialog
					title="Delete access point"
					submit="Delete access point"
					confirmation={dialog.point.AccessPointId}
					onClose={close}
					onSaved={() => {
						if (selected === dialog.point.AccessPointId) setSelected("");
						saved();
					}}
					plan={async () => [
						{
							label: "DeleteAccessPoint",
							run: () =>
								efsCall(scope, "DeleteAccessPoint", {
									AccessPointId: dialog.point.AccessPointId,
								}),
						},
					]}
				>
					<Alert type="warning">
						Delete {dialog.point.Name || dialog.point.AccessPointId}. This
						cannot be undone.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
