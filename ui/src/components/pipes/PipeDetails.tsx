import {
	Alert,
	Box,
	ColumnLayout,
	Container,
	ExpandableSection,
	Header,
	Modal,
	SpaceBetween,
} from "@cloudscape-design/components";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import { unixTime } from "#/lib/format";
import { type PipesScope, pipesCall } from "#/lib/pipes";
import type { Pipe } from "#/lib/pipesHelpers";
import { usePoll } from "#/lib/usePoll";

export function PipeDetails({
	scope,
	name,
	enabled,
	onClose,
}: {
	scope: PipesScope;
	name: string;
	enabled: boolean;
	onClose: () => void;
}) {
	const poll = usePoll(() =>
		pipesCall<Pipe>(scope, "DescribePipe", { Name: name }),
	);
	const p = poll.data;
	const facts = p
		? [
				["Name", p.Name],
				["ARN", p.Arn],
				["Description", p.Description],
				["Current state", p.CurrentState],
				["Desired state", p.DesiredState],
				["State reason", p.StateReason],
				["Source", p.Source],
				["Enrichment", p.Enrichment],
				["Target", p.Target],
				["Role ARN", p.RoleArn],
				["Created", unixTime(p.CreationTime)],
				["Last modified", unixTime(p.LastModifiedTime)],
			]
		: [];
	return (
		<Modal
			visible
			size="max"
			header={`Pipe: ${name}`}
			closeAriaLabel="Close pipe details"
			onDismiss={onClose}
		>
			<SpaceBetween size="m">
				{!!poll.error && <Alert type="error">{poll.error}</Alert>}
				{!p && <Box>Loading pipe…</Box>}
				{p && (
					<SpaceBetween size="m">
						<Container header={<Header variant="h2">Configuration</Header>}>
							<ColumnLayout columns={2} variant="text-grid">
								{facts.map(([label, value]) => (
									<div key={label}>
										<Box variant="awsui-key-label">{label}</Box>
										<div>{value || "-"}</div>
									</div>
								))}
							</ColumnLayout>
						</Container>
						{(
							[
								["Source parameters", p.SourceParameters],
								["Enrichment parameters", p.EnrichmentParameters],
								["Target parameters", p.TargetParameters],
							] as const
						).map(([label, value]) => (
							<Container
								key={label}
								header={<Header variant="h2">{label}</Header>}
							>
								<CodeBlock
									text={value ? JSON.stringify(value, null, 2) : "-"}
								/>
							</Container>
						))}
						<ConsoleTags
							tags={p.Tags ?? {}}
							enabled={enabled}
							onChanged={poll.reload}
							plan={(next, removed) => [
								...(Object.keys(next).length
									? [
											{
												label: "TagResource",
												run: () =>
													pipesCall(scope, "TagResource", {
														ResourceArn: p.Arn,
														Tags: next,
													}),
											},
										]
									: []),
								...(removed.length
									? [
											{
												label: "UntagResource",
												run: () =>
													pipesCall(scope, "UntagResource", {
														ResourceArn: p.Arn,
														TagKeys: removed,
													}),
											},
										]
									: []),
							]}
						/>
						<ExpandableSection headerText="Raw JSON">
							<CodeBlock text={JSON.stringify(p, null, 2)} />
						</ExpandableSection>
					</SpaceBetween>
				)}
			</SpaceBetween>
		</Modal>
	);
}
