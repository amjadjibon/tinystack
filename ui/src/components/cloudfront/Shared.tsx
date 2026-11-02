import {
	Alert,
	CopyToClipboard,
	SpaceBetween,
} from "@cloudscape-design/components";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type CloudFrontScope,
	cloudfrontCall,
	listTags,
} from "#/lib/cloudfront";
import { tagItems, tagMap } from "#/lib/cloudfrontHelpers";
import { usePoll } from "#/lib/usePoll";

// Copy shows text that is worth pasting elsewhere, such as an ID or an ARN.
export function Copy({ text, label }: { text?: string; label: string }) {
	return text ? (
		<CopyToClipboard
			variant="inline"
			textToCopy={text}
			copySuccessText={`${label} copied`}
			copyErrorText={`${label} failed to copy`}
		/>
	) : (
		"-"
	);
}

// ResourceTags edits the tags of a distribution or key value store with its ARN.
export function ResourceTags({
	scope,
	arn,
	enabled,
}: {
	scope: CloudFrontScope;
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
										cloudfrontCall(scope, "TagResource", {
											Resource: arn,
											Tags: tagItems(next),
										}),
								},
							]
						: []),
					...(removed.length
						? [
								{
									label: "UntagResource",
									run: () =>
										cloudfrontCall(scope, "UntagResource", {
											Resource: arn,
											TagKeys: { Items: removed },
										}),
								},
							]
						: []),
				]}
			/>
		</SpaceBetween>
	);
}
