import { Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type ActionStep, integer } from "#/lib/consoleActions";
import type { StreamCall, StreamDescription } from "#/lib/streamingHelpers";
import { Choice, Field } from "./Fields";
export function KinesisSettings({
	kind,
	description: d,
	shardCount,
	call,
	onClose,
	onSaved,
}: {
	kind: string;
	description: StreamDescription;
	shardCount: number;
	call: StreamCall;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [count, setCount] = useState(String(shardCount)),
		[retention, setRetention] = useState(String(d.RetentionPeriodHours ?? 24)),
		[encryption, setEncryption] = useState(d.EncryptionType ?? "NONE"),
		[key, setKey] = useState(d.KeyId ?? "alias/aws/kinesis");
	const metrics = [
		"IncomingBytes",
		"IncomingRecords",
		"OutgoingBytes",
		"OutgoingRecords",
		"WriteProvisionedThroughputExceeded",
		"ReadProvisionedThroughputExceeded",
		"IteratorAgeMilliseconds",
	];
	const previous =
		d.EnhancedMonitoring?.flatMap((m) => m.ShardLevelMetrics ?? []) ?? [];
	const [selected, setSelected] = useState<string[]>(
		previous.includes("ALL") ? metrics : previous,
	);
	return (
		<ActionDialog
			title={`Update ${kind}`}
			onClose={onClose}
			onSaved={onSaved}
			confirmation={
				kind === "retention" || (kind === "encryption" && encryption === "NONE")
					? "update"
					: undefined
			}
			plan={async () => {
				if (kind === "shards")
					return [
						{
							label: "Update shard count",
							run: () =>
								call("UpdateShardCount", {
									TargetShardCount: integer(count, "Shard count", 1, 500),
									ScalingType: "UNIFORM_SCALING",
								}),
						},
					];
				if (kind === "retention") {
					const hours = integer(retention, "Retention hours", 24, 8760);
					return hours === d.RetentionPeriodHours
						? []
						: [
								{
									label: "Update retention",
									run: () =>
										call(
											hours > (d.RetentionPeriodHours ?? 24)
												? "IncreaseStreamRetentionPeriod"
												: "DecreaseStreamRetentionPeriod",
											{ RetentionPeriodHours: hours },
										),
								},
							];
				}
				if (kind === "encryption") {
					if (!key.trim()) throw new Error("Enter a KMS key ID.");
					return [
						{
							label: "Update encryption",
							run: () =>
								call(
									encryption === "KMS"
										? "StartStreamEncryption"
										: "StopStreamEncryption",
									{ EncryptionType: "KMS", KeyId: key },
								),
						},
					];
				}
				const before = previous.includes("ALL") ? metrics : previous;
				const remove = before.filter((m) => !selected.includes(m)),
					add = selected.filter((m) => !before.includes(m));
				const steps: ActionStep[] = [];
				if (remove.length)
					steps.push({
						label: "Disable metrics",
						run: () =>
							call("DisableEnhancedMonitoring", { ShardLevelMetrics: remove }),
					});
				if (add.length)
					steps.push({
						label: "Enable metrics",
						run: () =>
							call("EnableEnhancedMonitoring", { ShardLevelMetrics: add }),
					});
				return steps;
			}}
		>
			<SpaceBetween size="m">
				{kind === "shards" && (
					<Field
						label="Target shard count"
						value={count}
						onChange={setCount}
						type="number"
					/>
				)}
				{kind === "retention" && (
					<Field
						label="Retention (hours)"
						value={retention}
						onChange={setRetention}
						type="number"
					/>
				)}
				{kind === "encryption" && (
					<>
						<Choice
							label="Encryption"
							value={encryption}
							onChange={setEncryption}
							options={["NONE", "KMS"]}
						/>
						<Field label="KMS key ID" value={key} onChange={setKey} />
					</>
				)}
				{kind === "monitoring" &&
					metrics.map((metric) => (
						<Checkbox
							key={metric}
							checked={selected.includes(metric)}
							onChange={({ detail }) =>
								setSelected((values) =>
									detail.checked
										? [...values, metric]
										: values.filter((m) => m !== metric),
								)
							}
						>
							{metric}
						</Checkbox>
					))}
			</SpaceBetween>
		</ActionDialog>
	);
}
