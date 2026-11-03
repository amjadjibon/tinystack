import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	listTags,
	type OpenSearchScope,
	opensearchCall,
} from "#/lib/opensearch";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";

// DomainTags edits the tags of a domain, which the service lists and changes by ARN.
export function DomainTags({
	scope,
	arn,
	enabled,
}: {
	scope: OpenSearchScope;
	arn: string;
	enabled: boolean;
}) {
	const poll = usePoll(() => listTags(scope, arn), 0);
	return (
		<SpaceBetween size="s">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<ConsoleTags
				tags={poll.data ?? {}}
				enabled={enabled}
				onChanged={poll.reload}
				plan={(next, removed) => [
					...(Object.keys(next).length
						? [
								{
									label: "AddTags",
									run: () =>
										opensearchCall(scope, "AddTags", {
											ARN: arn,
											TagList: tagList(next),
										}),
								},
							]
						: []),
					...(removed.length
						? [
								{
									label: "RemoveTags",
									run: () =>
										opensearchCall(scope, "RemoveTags", {
											ARN: arn,
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
