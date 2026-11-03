import { ConsoleTags } from "#/components/ConsoleTags";
import { type RDSScope, rdsCall, type Tag } from "#/lib/rds";
import { tagList } from "#/lib/tags";
export function RDSTags({
	scope,
	arn,
	tags,
	enabled,
	onChanged,
}: {
	scope: RDSScope;
	arn: string;
	tags?: Tag[];
	enabled: boolean;
	onChanged: () => void;
}) {
	return (
		<ConsoleTags
			tags={Object.fromEntries((tags ?? []).map((t) => [t.Key, t.Value]))}
			enabled={enabled}
			onChanged={onChanged}
			plan={(next, removed) => [
				...(removed.length
					? [
							{
								label: "Remove tags",
								run: () =>
									rdsCall(scope, "RemoveTagsFromResource", {
										ResourceName: arn,
										TagKeys: removed,
									}),
							},
						]
					: []),
				...(Object.keys(next).length
					? [
							{
								label: "Save tags",
								run: () =>
									rdsCall(scope, "AddTagsToResource", {
										ResourceName: arn,
										Tags: tagList(next),
									}),
							},
						]
					: []),
			]}
		/>
	);
}
