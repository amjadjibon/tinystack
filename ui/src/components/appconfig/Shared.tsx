import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleTags } from "#/components/ConsoleTags";
import { Text } from "#/components/memorydb/Fields";
import { type AppConfigScope, appconfigCall, listTags } from "#/lib/appconfig";
import type { NamedForm } from "#/lib/appconfigHelpers";
import { usePoll } from "#/lib/usePoll";

// ResourceTags edits the tags of a resource with its ARN.
export function ResourceTags({
	scope,
	arn,
	enabled,
}: {
	scope: AppConfigScope;
	arn: string;
	enabled: boolean;
}) {
	const poll = usePoll(() => listTags(scope, arn), 0);
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
										appconfigCall(scope, "TagResource", {
											ResourceArn: arn,
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
										appconfigCall(scope, "UntagResource", {
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

// NameFields asks for the name and description every AppConfig resource but a version has.
export function NameFields({
	value,
	onChange,
}: {
	value: NamedForm;
	onChange: (patch: Partial<NamedForm>) => void;
}) {
	return (
		<SpaceBetween size="m">
			<Text
				label="Name"
				value={value.name}
				onChange={(name) => onChange({ name })}
				constraint="1 to 64 characters. Unique among its siblings."
			/>
			<Text
				label="Description"
				value={value.description}
				onChange={(description) => onChange({ description })}
				constraint="Optional, up to 1024 characters."
			/>
		</SpaceBetween>
	);
}

// DeleteDialog deletes one resource once its name has been typed.
export function DeleteDialog({
	scope,
	title,
	noun,
	confirmation,
	operation,
	input,
	warning,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	title: string;
	noun: string;
	// The text the person types to confirm: the name of what is deleted.
	confirmation: string;
	operation: string;
	input: Record<string, unknown>;
	warning: string;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title={title}
			submit={`Delete ${noun}`}
			confirmation={confirmation}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{ label: operation, run: () => appconfigCall(scope, operation, input) },
			]}
		>
			<Alert type="warning">{warning}</Alert>
		</ActionDialog>
	);
}
