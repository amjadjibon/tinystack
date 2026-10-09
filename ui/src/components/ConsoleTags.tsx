import {
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
}: {
	tags: Record<string, string>;
	enabled: boolean;
	plan: (next: Record<string, string>, removed: string[]) => ActionStep[];
	onChanged: () => void;
}) {
	const [editing, setEditing] = useState(false),
		[text, setText] = useState("{}");
	return (
		<Container
			header={
				<Header
					variant="h2"
					counter={`(${Object.keys(tags).length})`}
					actions={
						<Button
							disabled={!enabled}
							onClick={() => {
								setText(JSON.stringify(tags, null, 2));
								setEditing(true);
							}}
						>
							Edit tags
						</Button>
					}
				>
					Tags
				</Header>
			}
		>
			<CodeBlock text={JSON.stringify(tags, null, 2)} />
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
		</Container>
	);
}
