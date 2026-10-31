import { Alert, Button } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type CfnScope, cfnCall } from "#/lib/cloudformation";
import { inProgress, type Stack } from "#/lib/cloudformationHelpers";
export function StackProtection({
	scope,
	stack,
	onChanged,
}: {
	scope: CfnScope;
	stack: Stack;
	onChanged: () => void;
}) {
	const [open, setOpen] = useState(false);
	const enable = !stack.EnableTerminationProtection;
	return (
		<>
			<Button
				disabled={
					!!stack.ParentId ||
					inProgress(stack.StackStatus) ||
					stack.StackStatus === "DELETE_COMPLETE"
				}
				onClick={() => setOpen(true)}
			>
				{enable ? "Enable" : "Disable"} termination protection
			</Button>
			{open && (
				<ActionDialog
					title="Termination protection"
					onClose={() => setOpen(false)}
					onSaved={() => {
						setOpen(false);
						onChanged();
					}}
					plan={async () => [
						{
							label: "Update termination protection",
							run: () =>
								cfnCall(scope, "UpdateTerminationProtection", {
									StackName: stack.StackId,
									EnableTerminationProtection: enable,
								}),
						},
					]}
				>
					<Alert>
						{enable
							? "Protect this stack from deletion."
							: "Allow deletion of this stack and its resources."}
					</Alert>
				</ActionDialog>
			)}
		</>
	);
}
