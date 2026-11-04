import {
	Alert,
	Button,
	Container,
	Header,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { errorText } from "#/lib/consoleActions";
import {
	type DynamoScope,
	dynamoCall,
	listExports,
	tableArn,
} from "#/lib/dynamodb";
import { usePoll } from "#/lib/usePoll";
import { DynamoAction } from "./DynamoAction";

export function DynamoExports({
	scope,
	enabled,
}: {
	scope: DynamoScope;
	enabled: boolean;
}) {
	const exports = usePoll(async () => (enabled ? listExports(scope) : []), 0);
	const [detail, setDetail] = useState<unknown>();
	const [error, setError] = useState("");
	return (
		<SpaceBetween size="m">
			{!!(exports.error || error) && (
				<Alert type="error">{exports.error || error}</Alert>
			)}
			<Alert>
				Exports contain the current table state in DynamoDB JSON format. The
				table needs point-in-time recovery, and the S3 bucket must already
				exist.
			</Alert>
			<Table
				items={exports.data ?? []}
				loading={exports.loading}
				empty="No exports."
				header={
					<Header
						variant="h2"
						actions={
							<SpaceBetween direction="horizontal" size="xs">
								<Button onClick={exports.reload}>Refresh exports</Button>
								<DynamoAction
									label="Export to S3"
									enabled={enabled}
									fields={[
										{
											key: "bucket",
											label: "S3 bucket",
											validate: (value) => {
												if (!value.trim())
													throw new Error("Enter an S3 bucket.");
											},
										},
										{ key: "prefix", label: "S3 prefix (optional)" },
										{
											key: "format",
											label: "Format",
											initial: "DYNAMODB_JSON",
											validate: (value) => {
												if (value !== "DYNAMODB_JSON")
													throw new Error("Only DYNAMODB_JSON is supported.");
											},
										},
									]}
									run={(v) => {
										if (!v.bucket.trim())
											throw new Error("Enter an S3 bucket.");
										if (v.format !== "DYNAMODB_JSON")
											throw new Error("Only DYNAMODB_JSON is supported.");
										return dynamoCall(scope, "ExportTableToPointInTime", {
											TableArn: tableArn(scope),
											S3Bucket: v.bucket.trim(),
											S3Prefix: v.prefix,
											ExportFormat: v.format,
										});
									}}
									onSaved={exports.reload}
								/>
							</SpaceBetween>
						}
					>
						Exports
					</Header>
				}
				columnDefinitions={[
					{ id: "arn", header: "ARN", cell: (e) => e.ExportArn },
					{ id: "status", header: "Status", cell: (e) => e.ExportStatus },
					{
						id: "details",
						header: "Details",
						cell: (e) => (
							<Button
								disabled={!enabled}
								onClick={async () => {
									try {
										setDetail(
											await dynamoCall(scope, "DescribeExport", {
												ExportArn: e.ExportArn,
											}),
										);
										setError("");
									} catch (err) {
										setError(errorText(err));
									}
								}}
							>
								Details
							</Button>
						),
					},
				]}
			/>
			{detail !== undefined && (
				<Container header={<Header variant="h2">Export details</Header>}>
					<CodeBlock text={JSON.stringify(detail, null, 2)} />
				</Container>
			)}
		</SpaceBetween>
	);
}
