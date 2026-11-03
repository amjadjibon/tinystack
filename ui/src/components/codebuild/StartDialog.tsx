import { SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Field } from "#/components/containers/Controls";
import { codebuildCall } from "#/lib/codebuild";
import { type Scope, variables } from "#/lib/codebuildHelpers";
export function StartDialog({
	scope,
	name,
	onClose,
	onSaved,
}: {
	scope: Scope;
	name: string;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [version, setVersion] = useState(""),
		[vars, setVars] = useState("[]");
	return (
		<ActionDialog
			title={`Start build: ${name}`}
			submit="Start build"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = {
					ProjectName: name,
					...(version ? { SourceVersion: version } : {}),
					EnvironmentVariablesOverride: variables(vars),
				};
				return [
					{
						label: "StartBuild",
						run: () => codebuildCall(scope, "StartBuild", input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Field
					label="Source version override (leave empty for project version)"
					value={version}
					onChange={setVersion}
				/>
				<Field
					label="Environment overrides (JSON array: Name, Value, Type; use references for PARAMETER_STORE/SECRETS_MANAGER)"
					multiline
					value={vars}
					onChange={setVars}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
