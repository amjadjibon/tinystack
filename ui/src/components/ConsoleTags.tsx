import {
	Box,
	Button,
	Container,
	FormField,
	Header,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import { type ActionStep, stringMap } from "#/lib/consoleActions";
import { ActionDialog } from "./ActionDialog";
import { CodeBlock } from "./CodeBlock";
export function ConsoleTags({
	tags,
	enabled,
	plan,
	onChanged,
	compact = false,
}: {
	compact?: boolean;
	tags: Record<string, string>;
	enabled: boolean;
	plan: (next: Record<string, string>, removed: string[]) => ActionStep[];
	onChanged: () => void;
}) {
	const [editing, setEditing] = useState(false),
		[text, setText] = useState("{}");
	const editButton = (
		<Button
			disabled={!enabled}
			onClick={() => {
				setText(JSON.stringify(tags, null, 2));
				setEditing(true);
			}}
		>
			Edit tags
		</Button>
	);
	return (
		<>
			{compact ? (
				editButton
			) : (
				<Container
					header={
						<Header
							variant="h2"
							counter={`(${Object.keys(tags).length})`}
							actions={editButton}
						>
							Tags
						</Header>
					}
				>
					{Object.keys(tags).length ? (
						<CodeBlock text={JSON.stringify(tags, null, 2)} />
					) : (
						<Box>No tags</Box>
					)}
				</Container>
			)}

			{editing && (
				<ActionDialog
					title="Edit tags"
					onClose={() => setEditing(false)}
					onSaved={() => {
						setEditing(false);
						onChanged();
					}}
					plan={async () => {
						const next = stringMap(text, "Tags");
						return plan(
							next,
							Object.keys(tags).filter((k) => !Object.hasOwn(next, k)),
						);
					}}
				>
					<FormField
						label="Tags (JSON)"
						description="Object of string values. Use {} to remove all tags."
					>
						<Textarea
							ariaLabel="Resource tags"
							rows={8}
							value={text}
							onChange={({ detail }) => setText(detail.value)}
						/>
					</FormField>
				</ActionDialog>
			)}
		</>
	);
}
