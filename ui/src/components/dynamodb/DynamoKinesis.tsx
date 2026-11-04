import {
	Alert,
	Button,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import {
	type DynamoScope,
	type KinesisDestination,
	tableCall,
} from "#/lib/dynamodb";
import { usePoll } from "#/lib/usePoll";
import { DynamoAction } from "./DynamoAction";

export function DynamoKinesis({
	scope,
	enabled,
}: {
	scope: DynamoScope;
	enabled: boolean;
}) {
	const destinations = usePoll(
		async () =>
			enabled
				? ((
						await tableCall<{
							KinesisDataStreamDestinations?: KinesisDestination[];
						}>(scope, "DescribeKinesisStreamingDestination")
					).KinesisDataStreamDestinations ?? [])
				: [],
		0,
	);
	const precision = (value: string) => {
		if (!["MILLISECOND", "MICROSECOND"].includes(value))
			throw new Error("Precision must be MILLISECOND or MICROSECOND.");
		return { ApproximateCreationDateTimePrecision: value };
	};
	const enable = (arn: string, value: string) => {
		if (!arn.trim()) throw new Error("Enter a Kinesis stream ARN.");
		return tableCall(scope, "EnableKinesisStreamingDestination", {
			StreamArn: arn.trim(),
			EnableKinesisStreamingConfiguration: precision(value),
		});
	};
	return (
		<SpaceBetween size="m">
			{!!destinations.error && <Alert type="error">{destinations.error}</Alert>}
			<Table
				items={destinations.data ?? []}
				loading={destinations.loading}
				empty="No Kinesis destinations."
				header={
					<Header
						variant="h2"
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button onClick={destinations.reload}>
									Refresh destinations
								</Button>
								<DynamoAction
									label="Enable destination"
									enabled={enabled}
									fields={[
										{
											key: "arn",
											label: "Existing Kinesis stream ARN",
											validate: (value) => {
												if (!value.trim())
													throw new Error("Enter a Kinesis stream ARN.");
											},
										},
										{
											key: "precision",
											label: "Timestamp precision",
											validate: precision,
											initial: "MILLISECOND",
										},
									]}
									run={(v) => enable(v.arn, v.precision)}
									onSaved={destinations.reload}
								/>
							</SpaceBetween>
						}
					>
						Kinesis streaming destinations
					</Header>
				}
				columnDefinitions={[
					{ id: "arn", header: "Stream ARN", cell: (d) => d.StreamArn },
					{
						id: "status",
						header: "Status",
						cell: (d) =>
							`${d.DestinationStatus} ${d.DestinationStatusDescription ?? ""}`,
					},
					{
						id: "precision",
						header: "Timestamp precision",
						cell: (d) => d.ApproximateCreationDateTimePrecision,
					},
					{
						id: "actions",
						header: "Actions",
						cell: (d) => (
							<SpaceBetween direction="horizontal" size="xs">
								<DynamoAction
									label={
										d.DestinationStatus === "ACTIVE" ? "Disable" : "Enable"
									}
									enabled={enabled}
									run={() =>
										d.DestinationStatus === "ACTIVE"
											? tableCall(scope, "DisableKinesisStreamingDestination", {
													StreamArn: d.StreamArn,
												})
											: enable(
													d.StreamArn,
													d.ApproximateCreationDateTimePrecision,
												)
									}
									onSaved={destinations.reload}
								/>
								<DynamoAction
									label="Update precision"
									enabled={enabled && d.DestinationStatus === "ACTIVE"}
									fields={[
										{
											key: "precision",
											label: "Timestamp precision",
											validate: precision,
											initial: d.ApproximateCreationDateTimePrecision,
										},
									]}
									run={(v) =>
										tableCall(scope, "UpdateKinesisStreamingDestination", {
											StreamArn: d.StreamArn,
											UpdateKinesisStreamingConfiguration: precision(
												v.precision,
											),
										})
									}
									onSaved={destinations.reload}
								/>
							</SpaceBetween>
						),
					},
				]}
			/>
		</SpaceBetween>
	);
}
