import { Alert, Box, Modal, SpaceBetween } from "@cloudscape-design/components";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Rows } from "#/components/containers/Controls";
import { unixTime } from "#/lib/format";
import { type GlueScope, glueCall, gluePages } from "#/lib/glue";
import {
	type GlueContext,
	type GlueKind,
	type GlueRecord,
	glueIdentity,
	glueName,
} from "#/lib/glueHelpers";
import { usePoll } from "#/lib/usePoll";
export function GlueDetails({
	scope,
	kind,
	row,
	context,
	enabled,
	onClose,
}: {
	scope: GlueScope;
	kind: GlueKind;
	row: GlueRecord;
	context: GlueContext;
	enabled: boolean;
	onClose: () => void;
}) {
	const name = glueName(row);
	const arn = `arn:aws:glue:${scope.region}:${scope.account}:${kind.toLowerCase()}/${name}`;
	const taggable = !["Table", "Partition"].includes(kind);
	const poll = usePoll(async () => {
		const [detail, tags, runs] = await Promise.all([
			glueCall<Record<string, GlueRecord>>(
				scope,
				`Get${kind}`,
				glueIdentity(kind, row, context),
			),
			taggable
				? glueCall<{ Tags: Record<string, string> }>(scope, "GetTags", {
						ResourceArn: arn,
					})
				: Promise.resolve({ Tags: {} }),
			kind === "Job"
				? gluePages<GlueRecord>(
						(input) => glueCall(scope, "GetJobRuns", input),
						"JobRuns",
						{ JobName: name },
					)
				: Promise.resolve([]),
		]);
		return { record: detail[kind], tags: tags.Tags, runs };
	});
	const p = poll.data?.record;
	const sd = p?.StorageDescriptor as { Columns?: GlueRecord[] } | undefined;
	const keys = (p?.PartitionKeys ?? []) as GlueRecord[];
	return (
		<Modal
			visible
			size="max"
			header={`${kind}: ${name}`}
			closeAriaLabel="Close details"
			onDismiss={onClose}
		>
			<SpaceBetween size="m">
				{poll.error && <Alert type="error">{poll.error}</Alert>}
				{!p && <Box>Loading…</Box>}
				{p && (
					<SpaceBetween size="m">
						{Object.entries(p)
							.filter(([k]) =>
								[
									"CreateTime",
									"UpdateTime",
									"CreatedOn",
									"LastModifiedOn",
									"CreationTime",
									"LastAccessTime",
								].includes(k),
							)
							.map(([k, v]) => (
								<div key={k}>
									<Box variant="awsui-key-label">{k}</Box>
									{unixTime(v as number)}
								</div>
							))}
						{kind === "Table" && (
							<Rows<GlueRecord & { Kind: string }>
								title="Schema"
								items={[
									...(sd?.Columns ?? []).map((c) => ({ ...c, Kind: "Column" })),
									...keys.map((c) => ({ ...c, Kind: "Partition key" })),
								]}
								emptyText="Edit this table to add columns."
								columns={[
									{
										id: "name",
										header: "Name",
										cell: (c) => String(c.Name ?? "-"),
									},
									{
										id: "type",
										header: "Type",
										cell: (c) => String(c.Type ?? "-"),
									},
									{ id: "kind", header: "Kind", cell: (c) => c.Kind },
									{
										id: "comment",
										header: "Comment",
										cell: (c) => String(c.Comment ?? "-"),
									},
								]}
							/>
						)}
						{kind === "Job" && (
							<Rows
								title="Job runs"
								items={poll.data?.runs ?? []}
								emptyText="Start a run from the jobs list."
								columns={[
									{ id: "id", header: "Run ID", cell: (r) => String(r.Id) },
									{
										id: "state",
										header: "Status",
										cell: (r) => String(r.JobRunState ?? "-"),
									},
									{
										id: "start",
										header: "Started",
										cell: (r) => unixTime(r.StartedOn as number),
									},
									{
										id: "end",
										header: "Completed",
										cell: (r) => unixTime(r.CompletedOn as number),
									},
									{
										id: "error",
										header: "Error",
										cell: (r) => String(r.ErrorMessage ?? "-"),
									},
								]}
							/>
						)}
						{taggable && (
							<ConsoleTags
								tags={poll.data?.tags ?? {}}
								enabled={enabled}
								onChanged={poll.reload}
								plan={(next, removed) => [
									...(Object.keys(next).length
										? [
												{
													label: "TagResource",
													run: () =>
														glueCall(scope, "TagResource", {
															ResourceArn: arn,
															TagsToAdd: next,
														}),
												},
											]
										: []),
									...(removed.length
										? [
												{
													label: "UntagResource",
													run: () =>
														glueCall(scope, "UntagResource", {
															ResourceArn: arn,
															TagsToRemove: removed,
														}),
												},
											]
										: []),
								]}
							/>
						)}
						<CodeBlock text={JSON.stringify(p, null, 2)} />
					</SpaceBetween>
				)}
			</SpaceBetween>
		</Modal>
	);
}
