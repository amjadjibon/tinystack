import { SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { integer } from "#/lib/consoleActions";
import type {
	StreamCall,
	StreamDescription,
	StreamService,
} from "#/lib/streamingHelpers";
import { Choice, Field } from "./Fields";
export function StreamForm({
	service,
	call,
	description: initialDescription,
	onClose,
	onSaved,
}: {
	service: StreamService;
	call: StreamCall;
	description?: StreamDescription;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [description] = useState(initialDescription);
	const destination = description?.Destinations?.[0];
	const config =
		destination?.ExtendedS3DestinationDescription ??
		destination?.S3DestinationDescription;
	const [name, setName] = useState(""),
		[mode, setMode] = useState("PROVISIONED"),
		[shards, setShards] = useState("1"),
		[source, setSource] = useState("DirectPut"),
		[sourceArn, setSourceArn] = useState(""),
		[sourceRole, setSourceRole] = useState(""),
		[bucket, setBucket] = useState(config?.BucketARN ?? ""),
		[role, setRole] = useState(config?.RoleARN ?? ""),
		[prefix, setPrefix] = useState(config?.Prefix ?? ""),
		[size, setSize] = useState(String(config?.BufferingHints?.SizeInMBs ?? 5)),
		[interval, setInterval] = useState(
			String(config?.BufferingHints?.IntervalInSeconds ?? 300),
		),
		[compression, setCompression] = useState(
			config?.CompressionFormat ?? "UNCOMPRESSED",
		);
	const editing = !!description;
	return (
		<ActionDialog
			title={
				editing
					? "Update S3 destination"
					: service === "kinesis"
						? "Create stream"
						: "Create delivery stream"
			}
			submit={
				editing
					? "Update destination"
					: service === "kinesis"
						? "Create stream"
						: "Create delivery stream"
			}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const maxNameLength = service === "kinesis" ? 128 : 64;
				if (
					!editing &&
					(!/^[a-zA-Z0-9_.-]+$/.test(name) || name.length > maxNameLength)
				)
					throw new Error(
						`Enter a stream name using 1–${maxNameLength} letters, digits, underscores, periods or hyphens.`,
					);
				if (service === "kinesis")
					return [
						{
							label: "Create stream",
							run: () =>
								call("CreateStream", {
									StreamName: name,
									StreamModeDetails: { StreamMode: mode },
									...(mode === "PROVISIONED"
										? { ShardCount: integer(shards, "Shard count", 1, 500) }
										: {}),
								}),
						},
					];
				if (!/^arn:aws:s3:::[^/]+$/.test(bucket))
					throw new Error("Enter an S3 bucket ARN.");
				if (!/^arn:aws:iam::\d{12}:role\/.+/.test(role))
					throw new Error("Enter a destination IAM role ARN.");
				const target = {
					BucketARN: bucket,
					RoleARN: role,
					Prefix: prefix,
					BufferingHints: {
						SizeInMBs: integer(size, "Buffer size", 1, 128),
						IntervalInSeconds: integer(interval, "Buffer interval", 0, 900),
					},
					CompressionFormat: compression,
				};
				if (editing) {
					if (!destination || !description.VersionId)
						throw new Error(
							"Refresh the delivery stream before updating its destination.",
						);
					return [
						{
							label: "Update destination",
							run: () =>
								call("UpdateDestination", {
									CurrentDeliveryStreamVersionId: description.VersionId,
									DestinationId: destination.DestinationId,
									[destination.ExtendedS3DestinationDescription
										? "ExtendedS3DestinationUpdate"
										: "S3DestinationUpdate"]: target,
								}),
						},
					];
				}
				if (source === "KinesisStreamAsSource" && (!sourceArn || !sourceRole))
					throw new Error(
						"Enter the source Kinesis stream ARN and source role ARN.",
					);
				return [
					{
						label: "Create delivery stream",
						run: () =>
							call("CreateDeliveryStream", {
								DeliveryStreamName: name,
								DeliveryStreamType: source,
								S3DestinationConfiguration: target,
								...(source === "KinesisStreamAsSource"
									? {
											KinesisStreamSourceConfiguration: {
												KinesisStreamARN: sourceArn,
												RoleARN: sourceRole,
											},
										}
									: {}),
							}),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!editing && (
					<Field label="Stream name" value={name} onChange={setName} />
				)}
				{service === "kinesis" ? (
					<SpaceBetween size="m">
						<Choice
							label="Capacity mode"
							value={mode}
							onChange={setMode}
							options={["PROVISIONED", "ON_DEMAND"]}
						/>
						{mode === "PROVISIONED" && (
							<Field
								label="Shard count"
								type="number"
								value={shards}
								onChange={setShards}
							/>
						)}
					</SpaceBetween>
				) : (
					<SpaceBetween size="m">
						{!editing && (
							<SpaceBetween size="m">
								<Choice
									label="Source"
									value={source}
									onChange={setSource}
									options={["DirectPut", "KinesisStreamAsSource"]}
								/>
								{source === "KinesisStreamAsSource" && (
									<SpaceBetween size="m">
										<Field
											label="Kinesis stream ARN"
											value={sourceArn}
											onChange={setSourceArn}
										/>
										<Field
											label="Source role ARN"
											value={sourceRole}
											onChange={setSourceRole}
										/>
									</SpaceBetween>
								)}
							</SpaceBetween>
						)}
						<Field label="S3 bucket ARN" value={bucket} onChange={setBucket} />
						<Field
							label="Destination role ARN"
							value={role}
							onChange={setRole}
						/>
						<Field label="S3 prefix" value={prefix} onChange={setPrefix} />
						<Field
							label="Buffer size (MiB)"
							value={size}
							onChange={setSize}
							type="number"
						/>
						<Field
							label="Buffer interval (seconds)"
							value={interval}
							onChange={setInterval}
							type="number"
						/>
						<Choice
							label="Compression"
							value={compression}
							onChange={setCompression}
							options={["UNCOMPRESSED", "GZIP"]}
						/>
					</SpaceBetween>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
