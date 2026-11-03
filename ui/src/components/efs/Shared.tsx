import { CopyToClipboard } from "@cloudscape-design/components";
import { ConsoleTags } from "#/components/ConsoleTags";
import { type EFSScope, efsCall } from "#/lib/efs";
import type { Tag } from "#/lib/efsHelpers";
import { tagList, tagMap } from "#/lib/tags";

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

// ResourceTags edits the tags of a file system or an access point. Both
// descriptions carry their tags, so the owner reloads them when a change is saved.
export function ResourceTags({
	scope,
	id,
	tags,
	enabled,
	onChanged,
}: {
	scope: EFSScope;
	id: string;
	tags?: Tag[];
	enabled: boolean;
	onChanged: () => void;
}) {
	return (
		<ConsoleTags
			tags={tagMap(tags)}
			enabled={enabled}
			onChanged={onChanged}
			plan={(next, removed) => [
				...(Object.keys(next).length
					? [
							{
								label: "TagResource",
								run: () =>
									efsCall(scope, "TagResource", {
										ResourceId: id,
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
									efsCall(scope, "UntagResource", {
										ResourceId: id,
										TagKeys: removed,
									}),
							},
						]
					: []),
			]}
		/>
	);
}
