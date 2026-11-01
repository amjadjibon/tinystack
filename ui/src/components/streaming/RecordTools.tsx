import {
	Alert,
	Button,
	Checkbox,
	Container,
	FormField,
	Header,
	SpaceBetween,
	Table,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import {
	decodeRecord,
	encodeRecord,
	type Shard,
	type StreamCall,
	type StreamService,
} from "#/lib/streamingHelpers";
import { Choice, Field } from "./Fields";
export function RecordTools({
	service,
	call,
	shards,
	enabled,
	direct,
}: {
	service: StreamService;
	call: StreamCall;
	shards: Shard[];
	enabled: boolean;
	direct: boolean;
}) {
	const [put, setPut] = useState(false),
		[data, setData] = useState(""),
		[partition, setPartition] = useState("console"),
		[result, setResult] = useState(""),
		[reveal, setReveal] = useState(false),
		// lastShard points the viewer at the shard that received the last put record.
		[lastShard, setLastShard] = useState("");
	return (
		<SpaceBetween size="m">
			<Container
				header={
					<Header
						variant="h2"
						actions={
							<Button
								disabled={!enabled || !direct}
								onClick={() => setPut(true)}
							>
								Put record
							</Button>
						}
					>
						Record tester
					</Header>
				}
			>
				{!direct && (
					<Alert type="info">
						This delivery stream reads from Kinesis. Put records into its source
						stream.
					</Alert>
				)}
				{result && <CodeBlock text={result} />}
			</Container>
			{put && (
				<ActionDialog
					title="Put record"
					submit="Put record"
					onClose={() => {
						setPut(false);
						setData("");
						setReveal(false);
					}}
					onSaved={() => {
						setPut(false);
						setData("");
						setReveal(false);
					}}
					plan={async () => {
						if (service === "kinesis" && !partition)
							throw new Error("Enter a partition key.");
						const encoded = encodeRecord(data);
						return [
							{
								label: "Put record",
								run: async () => {
									const out = await call(
										"PutRecord",
										service === "kinesis"
											? { Data: encoded, PartitionKey: partition }
											: { Record: { Data: encoded } },
									);
									setResult(JSON.stringify(out, null, 2));
									if (typeof out.ShardId === "string")
										setLastShard(out.ShardId);
								},
							},
						];
					}}
				>
					<SpaceBetween size="m">
						{service === "kinesis" && (
							<Field
								label="Partition key"
								value={partition}
								onChange={setPartition}
							/>
						)}
						<Checkbox
							checked={reveal}
							onChange={({ detail }) => setReveal(detail.checked)}
						>
							Reveal record data
						</Checkbox>
						{reveal ? (
							<FormField label="Record data (UTF-8)">
								<Textarea
									value={data}
									onChange={({ detail }) => setData(detail.value)}
								/>
							</FormField>
						) : (
							<Field
								label="Record data (UTF-8)"
								type="password"
								value={data}
								onChange={setData}
							/>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
			{service === "kinesis" && (
				<RecordViewer
					key={`${shards.map((s) => s.ShardId).join("/")}:${lastShard}`}
					call={call}
					shards={shards}
					initialShard={lastShard}
					enabled={enabled}
				/>
			)}
		</SpaceBetween>
	);
}
type RecordEntry = {
	SequenceNumber: string;
	PartitionKey: string;
	Data: string;
	ApproximateArrivalTimestamp?: number;
};
function RecordViewer({
	call,
	shards,
	initialShard,
	enabled,
}: {
	call: StreamCall;
	shards: Shard[];
	initialShard: string;
	enabled: boolean;
}) {
	const [shard, setShard] = useState(initialShard || shards[0]?.ShardId || ""),
		[start, setStart] = useState("TRIM_HORIZON"),
		[iterator, setIterator] = useState(""),
		[records, setRecords] = useState<RecordEntry[]>([]),
		[reveal, setReveal] = useState(false),
		[busy, setBusy] = useState(false),
		[error, setError] = useState("");
	const read = async (reset: boolean) => {
		setBusy(true);
		setError("");
		try {
			let cursor = iterator;
			if (reset) {
				setRecords([]);
				setIterator("");
				const out = await call<{ ShardIterator: string }>("GetShardIterator", {
					ShardId: shard,
					ShardIteratorType: start,
				});
				cursor = out.ShardIterator;
			}
			const out = await call<{
				Records?: RecordEntry[];
				NextShardIterator?: string;
			}>("GetRecords", {
				StreamName: undefined,
				ShardIterator: cursor,
				Limit: 100,
			});
			setRecords(out.Records ?? []);
			setIterator(out.NextShardIterator ?? "");
		} catch (err) {
			setError(err instanceof Error ? err.message : String(err));
		} finally {
			setBusy(false);
		}
	};
	const reset = () => {
		setIterator("");
		setRecords([]);
		setError("");
	};
	return (
		<Container header={<Header variant="h2">Records viewer</Header>}>
			<SpaceBetween size="m">
				{error && <Alert type="error">{error}</Alert>}
				<Choice
					disabled={busy}
					label="Shard"
					value={shard}
					options={shards.map((s) => s.ShardId)}
					onChange={(v) => {
						setShard(v);
						reset();
					}}
				/>
				<Choice
					disabled={busy}
					label="Starting position"
					value={start}
					options={["TRIM_HORIZON", "LATEST"]}
					onChange={(v) => {
						setStart(v);
						reset();
					}}
				/>
				<SpaceBetween direction="horizontal" size="s">
					<Button
						loading={busy}
						disabled={!enabled || !shard}
						onClick={() => read(true)}
					>
						Start reading
					</Button>
					<Button
						disabled={!enabled || !iterator || busy}
						onClick={() => read(false)}
					>
						Read next batch
					</Button>
				</SpaceBetween>
				<Checkbox
					checked={reveal}
					onChange={({ detail }) => setReveal(detail.checked)}
				>
					Reveal decoded record data
				</Checkbox>
				<Table
					items={records}
					loading={busy}
					empty="No records in this batch. Read the next batch or restart at TRIM_HORIZON."
					columnDefinitions={[
						{
							id: "sequence",
							header: "Sequence number",
							cell: (r) => r.SequenceNumber,
						},
						{
							id: "partition",
							header: "Partition key",
							cell: (r) => r.PartitionKey,
						},
						{
							id: "data",
							header: "Data (UTF-8)",
							cell: (r) =>
								reveal ? <CodeBlock text={decodeRecord(r.Data)} /> : "••••••••",
						},
					]}
				/>
			</SpaceBetween>
		</Container>
	);
}
