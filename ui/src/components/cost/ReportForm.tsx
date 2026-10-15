import { Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { configuration, costCall } from "#/lib/cost";

import { CostInput, CostSelect } from "./CostFields";
export function ReportForm({
	account,
	resource,
	onClose,
	onSaved,
}: {
	account: string;
	resource?: Resource;
	onClose: () => void;
	onSaved: () => void;
}) {
	const config = resource ? configuration(resource) : {};
	const [name, setName] = useState(resource?.name ?? "");
	const [bucket, setBucket] = useState(String(config.S3Bucket ?? "")),
		[prefix, setPrefix] = useState(String(config.S3Prefix ?? ""));
	const [region, setRegion] = useState(String(config.S3Region ?? "us-east-1"));
	const [time, setTime] = useState(String(config.TimeUnit ?? "DAILY"));
	const [format, setFormat] = useState(String(config.Format ?? "textORcsv")),
		[compression, setCompression] = useState(
			String(config.Compression ?? "GZIP"),
		);
	const [version, setVersion] = useState(
		String(config.ReportVersioning ?? "CREATE_NEW_REPORT"),
	);
	const [refresh, setRefresh] = useState(config.RefreshClosedReports === true);
	const [schema, setSchema] = useState(
			(config.AdditionalSchemaElements ?? []) as string[],
		),
		[artifacts, setArtifacts] = useState(
			(config.AdditionalArtifacts ?? []) as string[],
		);
	const toggle = (values: string[], value: string, checked: boolean) =>
		checked ? [...values, value] : values.filter((v) => v !== value);
	return (
		<ActionDialog
			title={resource ? "Modify report definition" : "Create report definition"}
			submit={resource ? "Save changes" : "Create report definition"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!/^[0-9A-Za-z!\-_.*'()]{1,256}$/.test(name))
					throw new Error(
						"Report name must contain 1–256 modeled characters (letters, numbers, ! - _ . * ' ( )).",
					);
				if (!/^[A-Za-z0-9_.-]{1,256}$/.test(bucket))
					throw new Error(
						"S3 bucket requires 1–256 letters, numbers, underscores, dots or hyphens.",
					);
				if (prefix.length > 256 || !/^[0-9A-Za-z!\-_.*'()/]*$/.test(prefix))
					throw new Error(
						"S3 prefix contains unsupported characters or exceeds 256 characters.",
					);
				if (
					![
						"af-south-1",
						"ap-east-1",
						"ap-south-1",
						"ap-south-2",
						"ap-southeast-1",
						"ap-southeast-2",
						"ap-southeast-3",
						"ap-northeast-1",
						"ap-northeast-2",
						"ap-northeast-3",
						"ca-central-1",
						"eu-central-1",
						"eu-central-2",
						"eu-west-1",
						"eu-west-2",
						"eu-west-3",
						"eu-north-1",
						"eu-south-1",
						"eu-south-2",
						"me-central-1",
						"me-south-1",
						"sa-east-1",
						"us-east-1",
						"us-east-2",
						"us-west-1",
						"us-west-2",
						"cn-north-1",
						"cn-northwest-1",
					].includes(region)
				)
					throw new Error("Provide a valid S3 Region.");
				const ReportDefinition = {
					...config,
					ReportName: name,
					S3Bucket: bucket,
					S3Prefix: prefix,
					S3Region: region,
					TimeUnit: time,
					Format: format,
					Compression: compression,
					AdditionalSchemaElements: schema,
					AdditionalArtifacts: artifacts,
					RefreshClosedReports: refresh,
					ReportVersioning: version,
				};
				const input = resource
					? { ReportName: resource.name, ReportDefinition }
					: { ReportDefinition };
				return [
					{
						label: resource
							? "Modify report definition"
							: "Create report definition",
						run: () =>
							costCall(
								"costandusagereportservice",
								account,
								resource ? "ModifyReportDefinition" : "PutReportDefinition",
								input,
							),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<CostInput
					label="Report name"
					value={name}
					onChange={setName}
					disabled={!!resource}
				/>
				<CostInput label="S3 bucket" value={bucket} onChange={setBucket} />
				<CostInput label="S3 prefix" value={prefix} onChange={setPrefix} />
				<CostInput label="S3 Region" value={region} onChange={setRegion} />
				<CostSelect
					label="Time unit"
					value={time}
					options={["HOURLY", "DAILY", "MONTHLY"]}
					onChange={setTime}
				/>
				<CostSelect
					label="Format"
					value={format}
					options={["textORcsv", "Parquet"]}
					onChange={setFormat}
				/>
				<CostSelect
					label="Compression"
					value={compression}
					options={["ZIP", "GZIP", "Parquet"]}
					onChange={setCompression}
				/>
				<CostSelect
					label="Report versioning"
					value={version}
					options={["CREATE_NEW_REPORT", "OVERWRITE_REPORT"]}
					onChange={setVersion}
				/>
				<Checkbox
					checked={refresh}
					onChange={({ detail }) => setRefresh(detail.checked)}
				>
					Refresh closed reports
				</Checkbox>
				{[
					"RESOURCES",
					"SPLIT_COST_ALLOCATION_DATA",
					"MANUAL_DISCOUNT_COMPATIBILITY",
				].map((value) => (
					<Checkbox
						key={value}
						checked={schema.includes(value)}
						onChange={({ detail }) =>
							setSchema(toggle(schema, value, detail.checked))
						}
					>
						{value}
					</Checkbox>
				))}
				{["REDSHIFT", "QUICKSIGHT", "ATHENA"].map((value) => (
					<Checkbox
						key={value}
						checked={artifacts.includes(value)}
						onChange={({ detail }) =>
							setArtifacts(toggle(artifacts, value, detail.checked))
						}
					>
						{value}
					</Checkbox>
				))}
			</SpaceBetween>
		</ActionDialog>
	);
}
