import {
	Alert,
	Button,
	Checkbox,
	Container,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleLayout } from "#/components/Console";
import { ConsoleTags } from "#/components/ConsoleTags";
import { ResourceTable } from "#/components/ResourceTable";
import type { Resource } from "#/lib/api";
import { firehoseCall } from "#/lib/firehose";
import { kinesisCall } from "#/lib/kinesis";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import {
	type StreamCall,
	type StreamDescription,
	type StreamScope,
	type StreamService,
	streamHref,
	streamNames,
	streamShards,
	streamTags,
} from "#/lib/streamingHelpers";
import { usePoll } from "#/lib/usePoll";
import { KinesisSettings } from "./KinesisSettings";
import { RecordTools } from "./RecordTools";
import { StreamForm } from "./StreamForm";
export function StreamingPage({
	service,
	stream,
}: {
	service: StreamService;
	stream: string;
}) {
	const { account, region, canWrite, capabilities } = useScope();
	const scope = { account, region, stream };
	const title =
		service === "kinesis" ? "Kinesis Data Streams" : "Data Firehose";
	const href = streamHref(service, { account, region });
	const enabled =
		!!capabilities.data?.pages[service] &&
		canWrite &&
		/^\d{12}$/.test(account) &&
		validRegion(region);
	return (
		<ConsoleLayout
			contentType={stream ? "default" : "table"}
			navigationHeader={{ text: title, href }}
			activeHref={href}
			navigationItems={[
				{ type: "link", text: "Streams", href },
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: title, href },
				...(stream ? [{ text: stream, href: streamHref(service, scope) }] : []),
			]}
			content={
				<SpaceBetween size="l">
					{!!capabilities.error && (
						<Alert type="error">{capabilities.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages[service] && (
						<Alert type="warning">
							This service is not enabled on this server.
						</Alert>
					)}
					{capabilities.data?.requires_signature && !canWrite && (
						<Alert type="info">
							Connect a root console session for this account to manage streams.
						</Alert>
					)}
					<Workspace
						key={`${service}/${account}/${region}/${stream}/${enabled}`}
						service={service}
						scope={scope}
						enabled={enabled}
					/>
				</SpaceBetween>
			}
		/>
	);
}
function Workspace({
	service,
	scope,
	enabled,
}: {
	service: StreamService;
	scope: StreamScope;
	enabled: boolean;
}) {
	const navigate = useNavigate(),
		{ inventory } = useScope();
	const [dialog, setDialog] = useState("");
	const [deleteConsumers, setDeleteConsumers] = useState(false);
	const call: StreamCall = (op, input) =>
		service === "kinesis"
			? kinesisCall(scope, op, input)
			: firehoseCall(scope, op, input);
	const list = usePoll(() =>
		enabled && !scope.stream
			? streamNames(call, service)
			: Promise.resolve(undefined),
	);
	const detail = usePoll(async () => {
		if (!enabled || !scope.stream) return undefined;
		const [out, tags, shards] = await Promise.all([
			call<{
				StreamDescription?: StreamDescription;
				DeliveryStreamDescription?: StreamDescription;
			}>(service === "kinesis" ? "DescribeStream" : "DescribeDeliveryStream"),
			streamTags(call, service),
			service === "kinesis" ? streamShards(call) : Promise.resolve([]),
		]);
		return {
			description: out.StreamDescription ?? out.DeliveryStreamDescription,
			tags,
			shards,
		};
	});
	const changed = () => {
		inventory.reload();
		list.reload();
		detail.reload();
	};
	const saved = () => {
		setDialog("");
		changed();
	};
	const scopedResources = (inventory.data ?? []).filter(
		(r) =>
			r.service === service &&
			r.account === scope.account &&
			r.region === scope.region,
	);
	const byName = new Map(scopedResources.map((r) => [r.name, r]));
	const resources: Resource[] =
		list.data?.map((name) => ({
			...byName.get(name),
			service,
			type: service === "kinesis" ? "stream" : "delivery-stream",
			name,
			account: scope.account,
			region: scope.region,
			arn: `arn:aws:${service}:${scope.region}:${scope.account}:${service === "kinesis" ? "stream" : "deliverystream"}/${name}`,
		})) ?? scopedResources;
	const d = detail.data?.description;
	return (
		<SpaceBetween size="l">
			{!!(list.error || detail.error || inventory.error) && (
				<Alert type="error">
					{list.error || detail.error || inventory.error}
				</Alert>
			)}
			{!scope.stream ? (
				<ResourceTable
					title={service === "kinesis" ? "Streams" : "Delivery streams"}
					defaultHidden={["type", "region", "account"]}
					headerActions={
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog("create")}
						>
							{service === "kinesis"
								? "Create stream"
								: "Create delivery stream"}
						</Button>
					}
					resources={resources}
					loading={list.loading}
					onRefresh={() => {
						list.reload();
						inventory.reload();
					}}
					emptyText="Create a stream to begin sending records in this account and Region."
				/>
			) : (
				<SpaceBetween size="l">
					<Header
						variant="h1"
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button
									onClick={detail.reload}
									iconName="refresh"
									ariaLabel="Refresh stream"
								/>
								<Button
									disabled={!enabled || !d}
									onClick={() => setDialog("delete")}
								>
									Delete stream
								</Button>
							</SpaceBetween>
						}
					>
						{scope.stream}
					</Header>
					{d && (
						<SpaceBetween size="l">
							<Container header={<Header variant="h2">Configuration</Header>}>
								<CodeBlock text={JSON.stringify(d, null, 2)} />
							</Container>
							{service === "kinesis" ? (
								<SpaceBetween size="l">
									<SpaceBetween direction="horizontal" size="s">
										{["shards", "retention", "encryption", "monitoring"].map(
											(kind) => (
												<Button
													key={kind}
													disabled={
														!enabled ||
														(kind === "shards" &&
															d.StreamModeDetails?.StreamMode === "ON_DEMAND")
													}
													onClick={() => setDialog(kind)}
												>
													Update {kind}
												</Button>
											),
										)}
									</SpaceBetween>
									<Table
										header={<Header variant="h2">Shards</Header>}
										items={detail.data?.shards ?? []}
										empty="No shards found."
										columnDefinitions={[
											{ id: "id", header: "Shard ID", cell: (s) => s.ShardId },
											{
												id: "hash",
												header: "Hash key range",
												cell: (s) =>
													`${s.HashKeyRange?.StartingHashKey} – ${s.HashKeyRange?.EndingHashKey}`,
											},
											{
												id: "sequence",
												header: "Sequence range",
												cell: (s) =>
													`${s.SequenceNumberRange?.StartingSequenceNumber} – ${s.SequenceNumberRange?.EndingSequenceNumber ?? "Open"}`,
											},
										]}
									/>
								</SpaceBetween>
							) : (
								<Button
									disabled={!enabled}
									onClick={() => setDialog("destination")}
								>
									Update destination
								</Button>
							)}
							<ConsoleTags
								tags={detail.data?.tags ?? {}}
								enabled={enabled}
								onChanged={changed}
								plan={(tags, removed) => [
									...(removed.length
										? [
												{
													label: "Remove tags",
													run: () =>
														call(
															service === "kinesis"
																? "RemoveTagsFromStream"
																: "UntagDeliveryStream",
															{ TagKeys: removed },
														),
												},
											]
										: []),
									...(Object.keys(tags).length
										? [
												{
													label: "Save tags",
													run: () =>
														call(
															service === "kinesis"
																? "AddTagsToStream"
																: "TagDeliveryStream",
															{
																Tags:
																	service === "kinesis"
																		? tags
																		: Object.entries(tags).map(
																				([Key, Value]) => ({ Key, Value }),
																			),
															},
														),
												},
											]
										: []),
								]}
							/>
							<RecordTools
								service={service}
								call={call}
								shards={detail.data?.shards ?? []}
								enabled={enabled}
								direct={
									service === "kinesis" || d.DeliveryStreamType === "DirectPut"
								}
							/>
						</SpaceBetween>
					)}
				</SpaceBetween>
			)}
			{(dialog === "create" || (dialog === "destination" && d)) && (
				<StreamForm
					service={service}
					call={call}
					description={dialog === "destination" ? d : undefined}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{d &&
				["shards", "retention", "encryption", "monitoring"].includes(
					dialog,
				) && (
					<KinesisSettings
						kind={dialog}
						description={d}
						shardCount={
							detail.data?.shards.filter(
								(s) => !s.SequenceNumberRange?.EndingSequenceNumber,
							).length ?? 0
						}
						call={call}
						onClose={() => setDialog("")}
						onSaved={saved}
					/>
				)}
			{dialog === "delete" && (
				<ActionDialog
					title="Delete stream"
					submit="Delete stream"
					confirmation={scope.stream}
					onClose={() => setDialog("")}
					onSaved={() => {
						inventory.reload();
						navigate({
							href: streamHref(service, {
								account: scope.account,
								region: scope.region,
							}),
						});
					}}
					plan={async () => [
						{
							label: "Delete stream",
							run: () =>
								call(
									service === "kinesis"
										? "DeleteStream"
										: "DeleteDeliveryStream",
									service === "kinesis"
										? { EnforceConsumerDeletion: deleteConsumers }
										: {},
								),
						},
					]}
				>
					{service === "kinesis" && (
						<Checkbox
							checked={deleteConsumers}
							onChange={({ detail }) => setDeleteConsumers(detail.checked)}
						>
							Also delete registered consumers
						</Checkbox>
					)}
					<Alert type="warning">
						This permanently deletes the stream and its stored records or
						delivery buffer.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
