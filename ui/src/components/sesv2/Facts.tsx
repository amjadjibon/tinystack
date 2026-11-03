import {
	Alert,
	Box,
	ColumnLayout,
	Container,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import type { ReactNode } from "react";
import { ConsoleTags } from "#/components/ConsoleTags";
import { listTags, type SESScope, sesv2Call } from "#/lib/sesv2";
import { tagList, tagMap } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";

export function Facts({
	title,
	actions,
	facts,
}: {
	title: string;
	actions?: ReactNode;
	facts: [string, ReactNode][];
}) {
	return (
		<Container
			header={
				<Header variant="h2" actions={actions}>
					{title}
				</Header>
			}
		>
			<ColumnLayout columns={3} variant="text-grid">
				{facts.map(([label, value]) => (
					<div key={label}>
						<Box variant="awsui-key-label">{label}</Box>
						<div>{value === "" || value == null ? "-" : value}</div>
					</div>
				))}
			</ColumnLayout>
		</Container>
	);
}

export function ResourceTags({
	scope,
	arn,
	enabled,
}: {
	scope: SESScope;
	arn: string;
	enabled: boolean;
}) {
	const poll = usePoll(async () => tagMap(await listTags(scope, arn)), 0);
	return (
		<SpaceBetween size="s">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<ConsoleTags
				tags={poll.data ?? {}}
				enabled={enabled}
				onChanged={poll.reload}
				plan={(next, removed) => [
					...(Object.keys(next).length
						? [
								{
									label: "TagResource",
									run: () =>
										sesv2Call(scope, "TagResource", {
											ResourceArn: arn,
											Tags: tagList(next),
										}),
								},
							]
						: []),
					...(removed.length
						? [
								{
									label: "UntagResource",
									run: () =>
										sesv2Call(scope, "UntagResource", {
											ResourceArn: arn,
											TagKeys: removed,
										}),
								},
							]
						: []),
				]}
			/>
		</SpaceBetween>
	);
}
