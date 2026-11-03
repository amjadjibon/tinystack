import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice, Field } from "#/components/containers/Controls";
import { codebuildCall } from "#/lib/codebuild";
import {
	type Project,
	projectForm,
	projectInput,
	type Scope,
} from "#/lib/codebuildHelpers";
export function ProjectDialog({
	scope,
	project,
	onClose,
	onSaved,
}: {
	scope: Scope;
	project?: Project;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState(projectForm(project));
	const set = (p: Partial<typeof f>) => setF((v) => ({ ...v, ...p }));
	return (
		<ActionDialog
			title={project ? "Update project" : "Create project"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = projectInput(f, project),
					op = project ? "UpdateProject" : "CreateProject";
				return [{ label: op, run: () => codebuildCall(scope, op, input) }];
			}}
		>
			<SpaceBetween size="m">
				{!project && (
					<Field
						label="Name"
						value={f.name}
						onChange={(name) => set({ name })}
					/>
				)}
				<Field
					label="Description"
					value={f.description}
					onChange={(description) => set({ description })}
				/>
				<Choice
					label="Source type"
					value={f.source}
					options={["NO_SOURCE", "S3", "GITHUB", "CODECOMMIT", "CODEPIPELINE"]}
					onChange={(source) => set({ source, sourceVersion: "" })}
				/>
				<Alert>
					Direct builds execute NO_SOURCE inline buildspecs or S3 ZIP sources.
					Git sources are metadata only; CODEPIPELINE sources run through
					CodePipeline.
				</Alert>
				{!["NO_SOURCE", "CODEPIPELINE"].includes(f.source) && (
					<Field
						label="Source location (S3 bucket/key or HTTPS clone URL)"
						value={f.location}
						onChange={(location) => set({ location })}
					/>
				)}
				<Field
					label="Buildspec (inline YAML or source-relative path)"
					multiline
					value={f.buildspec}
					onChange={(buildspec) => set({ buildspec })}
				/>
				{f.source !== "NO_SOURCE" && (
					<Field
						label="Source version"
						value={f.sourceVersion}
						onChange={(sourceVersion) => set({ sourceVersion })}
					/>
				)}
				<Choice
					label="Environment type"
					value={f.environment}
					options={["LINUX_CONTAINER"]}
					onChange={(environment) => set({ environment })}
				/>
				<Field
					label="Image (tag or SHA256 digest)"
					value={f.image}
					onChange={(image) => set({ image })}
				/>
				<Choice
					label="Compute class (metadata)"
					value={f.compute}
					options={[
						"BUILD_GENERAL1_SMALL",
						"BUILD_GENERAL1_MEDIUM",
						"BUILD_GENERAL1_LARGE",
					]}
					onChange={(compute) => set({ compute })}
				/>
				<Checkbox
					checked={f.privileged}
					onChange={({ detail }) => set({ privileged: detail.checked })}
				>
					Privileged mode
				</Checkbox>
				<Field
					label="Service role ARN"
					value={f.role}
					onChange={(role) => set({ role })}
				/>
				<Field
					label="Build timeout (5–2160 minutes)"
					value={f.timeout}
					onChange={(timeout) => set({ timeout })}
				/>
				<Field
					label="Queue timeout (5–480 minutes)"
					value={f.queued}
					onChange={(queued) => set({ queued })}
				/>
				<Field
					label="Environment variables (JSON array: Name, Value, Type; secret values are references)"
					multiline
					value={f.vars}
					onChange={(vars) => set({ vars })}
				/>
				<Field
					label="Artifacts (JSON; Type NO_ARTIFACTS, CODEPIPELINE or S3 with Location bucket, Name, optional Path, NamespaceType NONE/BUILD_ID, Packaging NONE/ZIP, OverrideArtifactName, EncryptionDisabled)"
					multiline
					value={f.artifacts}
					onChange={(artifacts) => set({ artifacts })}
				/>
				<Field
					label="CloudWatch logs (JSON: CloudWatchLogs with Status ENABLED/DISABLED, optional GroupName and StreamName prefix)"
					multiline
					value={f.logs}
					onChange={(logs) => set({ logs })}
				/>
				<Field
					label="Tags (JSON string map)"
					multiline
					value={f.tags}
					onChange={(tags) => set({ tags })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
