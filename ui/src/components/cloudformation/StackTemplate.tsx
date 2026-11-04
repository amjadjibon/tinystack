import {
	Alert,
	Box,
	Button,
	Header,
	SegmentedControl,
	SpaceBetween,
	Spinner,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { type CfnScope, cfnCall } from "#/lib/cloudformation";
import { usePoll } from "#/lib/usePoll";

const stages = ["Original", "Processed"];

// StackTemplate shows GetTemplate output for one stage. The template is read
// only; the console has no template editor.
export function StackTemplate({ scope }: { scope: CfnScope }) {
	const [stage, setStage] = useState(stages[0]);
	return (
		<SpaceBetween size="s">
			<Header
				actions={
					<SegmentedControl
						label="Template stage"
						selectedId={stage}
						onChange={({ detail }) => setStage(detail.selectedId)}
						options={stages.map((id) => ({ id, text: id }))}
					/>
				}
			>
				Template
			</Header>
			<TemplateBody key={stage} scope={scope} stage={stage} />
		</SpaceBetween>
	);
}

function TemplateBody({ scope, stage }: { scope: CfnScope; stage: string }) {
	const poll = usePoll(
		() =>
			cfnCall<{ TemplateBody?: unknown }>(scope, "GetTemplate", {
				StackName: scope.stack,
				TemplateStage: stage,
			}),
		0,
	);
	const body = poll.data?.TemplateBody;
	const text =
		typeof body === "string" ? body : body ? JSON.stringify(body, null, 2) : "";
	return (
		<SpaceBetween size="s">
			<Box float="right">
				<Button
					iconName="refresh"
					ariaLabel="Reload template"
					onClick={poll.reload}
				/>
			</Box>
			{!!poll.error && (
				<Alert type="error" header="Cannot load the template">
					{poll.error}
				</Alert>
			)}
			{poll.loading ? (
				<Box textAlign="center" padding="l">
					<Spinner /> Loading template
				</Box>
			) : (
				!poll.error && <CodeBlock text={text} />
			)}
		</SpaceBetween>
	);
}
