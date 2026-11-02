import {
	Alert,
	Container,
	Header,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Details, TagRowsField } from "#/components/elbv2/Shared";
import { Text } from "#/components/memorydb/Fields";
import {
	type CloudFrontScope,
	cloudfrontCall,
	deleteKeyValueStore,
	type KeyValueStore,
	listKeyValueStores,
	updateKeyValueStore,
} from "#/lib/cloudfront";
import {
	createKvsInput,
	kvsComment,
	kvsStatusType,
} from "#/lib/cloudfrontHelpers";
import { isoTime } from "#/lib/format";
import { usePoll } from "#/lib/usePoll";
import { Copy, ResourceTags } from "./Shared";

type Dialog =
	| { kind: "" }
	| { kind: "create" }
	| { kind: "edit" | "delete"; store: KeyValueStore };

function Status({ status }: { status?: string }) {
	return (
		<StatusIndicator type={kvsStatusType(status)}>
			{status ?? "-"}
		</StatusIndicator>
	);
}

export function KeyValueStores({
	scope,
	enabled,
	onChanged,
}: {
	scope: CloudFrontScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<Dialog>({ kind: "" });
	const [selected, setSelected] = useState("");
	const [name, setName] = useState("");
	const [comment, setComment] = useState("");
	const [tags, setTags] = useState<{ key: string; value: string }[]>([]);
	const poll = usePoll(() =>
		enabled ? listKeyValueStores(scope) : Promise.resolve([]),
	);
	const stores = poll.data ?? [];
	const detail = stores.find((s) => s.Name === selected);
	const close = () => setDialog({ kind: "" });
	const saved = () => {
		close();
		poll.reload();
		onChanged();
	};
	const open = (next: Dialog) => {
		setName("");
		setTags([]);
		setComment(next.kind === "edit" ? (next.store.Comment ?? "") : "");
		setDialog(next);
	};
	return (
		<SpaceBetween size="l">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows
				title="Key value stores"
				emptyText="Choose Create key value store to add one."
				items={stores}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled}
							onClick={() => open({ kind: "create" })}
						>
							Create key value store
						</Action>
					</Buttons>
				}
				columns={[
					{
						id: "name",
						header: "Name",
						cell: (s) => (
							<Action onClick={() => setSelected(s.Name ?? "")}>
								{s.Name ?? "-"}
							</Action>
						),
					},
					{
						id: "status",
						header: "Status",
						cell: (s) => <Status status={s.Status} />,
					},
					{ id: "comment", header: "Comment", cell: (s) => s.Comment || "-" },
					{
						id: "modified",
						header: "Last modified",
						cell: (s) => isoTime(s.LastModifiedTime),
					},
					{
						id: "actions",
						header: "Actions",
						minWidth: 260,
						cell: (s) => (
							<Buttons>
								<Action
									disabled={!enabled}
									onClick={() => open({ kind: "edit", store: s })}
								>
									Edit comment
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => open({ kind: "delete", store: s })}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{detail?.ARN && (
				<SpaceBetween size="l" key={detail.ARN}>
					<Container header={<Header variant="h2">{detail.Name}</Header>}>
						<Details
							items={[
								["Name", detail.Name],
								["Status", <Status key="status" status={detail.Status} />],
								["ID", <Copy key="id" text={detail.Id} label="ID" />],
								["ARN", <Copy key="arn" text={detail.ARN} label="ARN" />],
								["Comment", detail.Comment],
								["Last modified", isoTime(detail.LastModifiedTime)],
							]}
						/>
					</Container>
					<ResourceTags scope={scope} arn={detail.ARN} enabled={enabled} />
				</SpaceBetween>
			)}
			{dialog.kind === "create" && (
				<ActionDialog
					title="Create key value store"
					submit="Create key value store"
					onClose={close}
					onSaved={() => {
						setSelected(name.trim());
						saved();
					}}
					plan={async () => {
						const input = createKvsInput({ name, comment, tags });
						return [
							{
								label: "CreateKeyValueStore",
								run: () => cloudfrontCall(scope, "CreateKeyValueStore", input),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Text
							label="Name"
							value={name}
							onChange={setName}
							constraint="1 to 64 letters, digits, hyphens or underscores."
						/>
						<Text
							label="Comment"
							value={comment}
							onChange={setComment}
							constraint="Optional. Up to 128 characters."
						/>
						<TagRowsField rows={tags} onChange={setTags} />
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog.kind === "edit" && (
				<ActionDialog
					title={`Edit comment of ${dialog.store.Name}`}
					onClose={close}
					onSaved={saved}
					plan={async () => {
						const value = kvsComment(comment);
						const storeName = dialog.store.Name ?? "";
						return [
							{
								label: "UpdateKeyValueStore",
								run: () => updateKeyValueStore(scope, storeName, value),
							},
						];
					}}
				>
					<Text
						label="Comment"
						value={comment}
						onChange={setComment}
						constraint="Up to 128 characters."
					/>
				</ActionDialog>
			)}
			{dialog.kind === "delete" && (
				<ActionDialog
					title="Delete key value store"
					submit="Delete key value store"
					confirmation={dialog.store.Name}
					onClose={close}
					onSaved={() => {
						if (selected === dialog.store.Name) setSelected("");
						saved();
					}}
					plan={async () => {
						const storeName = dialog.store.Name ?? "";
						return [
							{
								label: "DeleteKeyValueStore",
								run: () => deleteKeyValueStore(scope, storeName),
							},
						];
					}}
				>
					<Alert type="warning">
						Delete {dialog.store.Name} and every key it holds. This cannot be
						undone.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
