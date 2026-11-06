import {
	Alert,
	Box,
	Button,
	Container,
	FormField,
	Header,
	Hotspot,
	Input,
	SpaceBetween,
	Table,
} from "@cloudscape-design/components";
import { useEffect, useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type Parameter,
	type SSMScope,
	ssmCall,
	ssmPages,
	ssmTagPlan,
} from "#/lib/ssm";
import { usePoll } from "#/lib/usePoll";
import { HOTSPOT } from "#/tutorials/hotspots";
import { ParameterForm } from "./ParameterForm";

function ParameterValue({
	scope,
	version,
	enabled,
}: {
	scope: SSMScope;
	version: number;
	enabled: boolean;
}) {
	const [value, setValue] = useState<string>(),
		[error, setError] = useState(""),
		[busy, setBusy] = useState(false);
	const generation = useRef(0);
	useEffect(
		() => () => {
			generation.current++;
		},
		[],
	);
	return (
		<SpaceBetween size="xs">
			{!!error && <Alert type="error">{error}</Alert>}
			{value === undefined ? <Box>••••••••</Box> : <CodeBlock text={value} />}
			<Hotspot hotspotId={HOTSPOT.parameterRead}>
				<Button
					disabled={!enabled}
					loading={busy}
					onClick={async () => {
						const request = ++generation.current;
						if (value !== undefined) {
							setValue(undefined);
							return;
						}
						setBusy(true);
						setError("");
						try {
							const out = await ssmCall<{ Parameter: { Value: string } }>(
								scope,
								"GetParameter",
								{ Name: `${scope.parameter}:${version}`, WithDecryption: true },
							);
							if (generation.current === request) setValue(out.Parameter.Value);
						} catch (err) {
							if (generation.current === request) setError(String(err));
						} finally {
							if (generation.current === request) setBusy(false);
						}
					}}
				>
					{value === undefined ? "Reveal value" : "Hide value"}
				</Button>
			</Hotspot>
		</SpaceBetween>
	);
}
export function ParameterDetails({
	scope,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: SSMScope;
	enabled: boolean;
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const [editing, setEditing] = useState(false),
		[deleting, setDeleting] = useState(false),
		[labelVersion, setLabelVersion] = useState<Parameter>(),
		[labels, setLabels] = useState("");
	const poll = usePoll(async () => {
		if (!enabled) return undefined;
		const [metadata, history, tags] = await Promise.all([
			ssmPages(scope, "DescribeParameters", {
				ParameterFilters: [
					{ Key: "Name", Option: "Equals", Values: [scope.parameter] },
				],
			}),
			ssmPages(scope, "GetParameterHistory", {
				Name: scope.parameter,
				WithDecryption: false,
			}),
			ssmCall<{ TagList?: { Key: string; Value: string }[] }>(
				scope,
				"ListTagsForResource",
				{ ResourceType: "Parameter", ResourceId: scope.parameter },
			),
		]);
		return {
			parameter: metadata[0],
			history,
			tags: Object.fromEntries(
				(tags.TagList ?? []).map((t) => [t.Key, t.Value]),
			),
		};
	}, 0);
	const changed = () => {
		poll.reload();
		onChanged();
	};
	const parameter = poll.data?.parameter;
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{enabled && !poll.loading && !poll.error && !parameter && (
				<Alert type="warning">
					Parameter not found in the selected account and Region.
				</Alert>
			)}
			{!enabled && (
				<Alert type="info">
					Connect a console session to load parameter details.
				</Alert>
			)}
			<Header
				variant="h1"
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button onClick={poll.reload}>Refresh</Button>
						<Button
							disabled={!enabled || !parameter}
							onClick={() => setEditing(true)}
						>
							Overwrite parameter
						</Button>
						<Button
							disabled={!enabled || !parameter}
							onClick={() => setDeleting(true)}
						>
							Delete parameter
						</Button>
					</SpaceBetween>
				}
			>
				{scope.parameter}
			</Header>
			{parameter && (
				<Container header={<Header variant="h2">Details</Header>}>
					<SpaceBetween size="m">
						<CodeBlock text={JSON.stringify(parameter, null, 2)} />
						<ParameterValue
							key={parameter.Version}
							scope={scope}
							version={parameter.Version}
							enabled={enabled}
						/>
					</SpaceBetween>
				</Container>
			)}
			<Table
				header={<Header variant="h2">Version history</Header>}
				loading={poll.loading}
				items={poll.data?.history ?? []}
				trackBy="Version"
				empty={<Box>No parameter versions.</Box>}
				columnDefinitions={[
					{ id: "version", header: "Version", cell: (p) => p.Version },
					{ id: "type", header: "Type", cell: (p) => p.Type },
					{ id: "tier", header: "Tier", cell: (p) => p.Tier },
					{
						id: "modified",
						header: "Last modified",
						cell: (p) =>
							p.LastModifiedDate
								? new Date(p.LastModifiedDate * 1000).toISOString()
								: "—",
					},
					{
						id: "labels",
						header: "Labels",
						cell: (p) => (
							<SpaceBetween size="xs">
								<Box>{p.Labels?.join(", ") || "—"}</Box>
								<Button
									disabled={!enabled}
									onClick={() => {
										setLabelVersion(p);
										setLabels(p.Labels?.join(", ") ?? "");
									}}
								>
									Edit labels
								</Button>
							</SpaceBetween>
						),
					},
					{
						id: "value",
						header: "Value",
						cell: (p) => (
							<ParameterValue
								key={p.Version}
								scope={scope}
								version={p.Version}
								enabled={enabled}
							/>
						),
					},
				]}
			/>
			<ConsoleTags
				tags={poll.data?.tags ?? {}}
				enabled={enabled && !!parameter}
				plan={(next, removed) => ssmTagPlan(scope, next, removed)}
				onChanged={changed}
			/>
			{editing && parameter && (
				<ParameterForm
					scope={scope}
					parameter={parameter}
					onClose={() => setEditing(false)}
					onSaved={() => {
						setEditing(false);
						changed();
					}}
				/>
			)}
			{deleting && (
				<ActionDialog
					title="Delete parameter"
					submit="Delete parameter"
					confirmation={scope.parameter}
					onClose={() => setDeleting(false)}
					onSaved={onDeleted}
					plan={async () => [
						{
							label: "Delete parameter",
							run: () =>
								ssmCall(scope, "DeleteParameter", { Name: scope.parameter }),
						},
					]}
				>
					<Box>
						Delete this parameter and all versions, labels and tags permanently.
					</Box>
				</ActionDialog>
			)}
			{labelVersion && (
				<ActionDialog
					title={`Edit labels for version ${labelVersion.Version}`}
					onClose={() => setLabelVersion(undefined)}
					onSaved={() => {
						setLabelVersion(undefined);
						changed();
					}}
					plan={async () => {
						const next = labels
							.split(",")
							.map((s) => s.trim())
							.filter(Boolean);
						if (new Set(next).size !== next.length || next.length > 10)
							throw new Error("Enter at most ten unique labels.");
						const removed = (labelVersion.Labels ?? []).filter(
							(l) => !next.includes(l),
						);
						const added = next.filter((l) => !labelVersion.Labels?.includes(l));
						const input = {
							Name: scope.parameter,
							ParameterVersion: labelVersion.Version,
						};
						return [
							...(removed.length
								? [
										{
											label: "Remove labels",
											run: () =>
												ssmCall(scope, "UnlabelParameterVersion", {
													...input,
													Labels: removed,
												}),
										},
									]
								: []),
							...(added.length
								? [
										{
											label: "Assign labels",
											run: async () => {
												const out = await ssmCall<{ InvalidLabels?: string[] }>(
													scope,
													"LabelParameterVersion",
													{ ...input, Labels: added },
												);
												if (out.InvalidLabels?.length)
													throw new Error(
														`Invalid labels: ${out.InvalidLabels.join(", ")}`,
													);
											},
										},
									]
								: []),
						];
					}}
				>
					<FormField
						label="Labels"
						description="Comma-separated labels. Assigning an existing label moves it to this version. Clear to remove labels."
					>
						<Input
							ariaLabel="Version labels"
							value={labels}
							onChange={({ detail }) => setLabels(detail.value)}
						/>
					</FormField>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
