import {
	Alert,
	Button,
	FormField,
	Multiselect,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type CfnScope, cfnCall } from "#/lib/cloudformation";
import {
	deleteFailed,
	inProgress,
	type Stack,
	type StackResource,
} from "#/lib/cloudformationHelpers";

// DeleteStack confirms by stack name. When the stack is DELETE_FAILED, the
// resources that failed can be retained so the deletion can finish.
export function DeleteStack({
	scope,
	stack,
	resources,
	onDeleted,
}: {
	scope: CfnScope;
	stack: Stack;
	resources: StackResource[];
	onDeleted: () => void;
}) {
	const [open, setOpen] = useState(false),
		[retain, setRetain] = useState<string[]>([]);
	const retaining = stack.StackStatus === deleteFailed;
	const options = resources.map((r) => ({
		value: r.LogicalResourceId,
		label: r.LogicalResourceId,
		description: r.ResourceType,
		tags: [r.ResourceStatus],
	}));
	return (
		<>
			<Button
				disabled={
					stack.StackStatus === "DELETE_COMPLETE" ||
					inProgress(stack.StackStatus)
				}
				onClick={() => {
					setRetain(
						resources
							.filter((r) => r.ResourceStatus === deleteFailed)
							.map((r) => r.LogicalResourceId),
					);
					setOpen(true);
				}}
			>
				Delete
			</Button>
			{open && (
				<ActionDialog
					title={`Delete stack ${stack.StackName}`}
					submit="Delete stack"
					confirmation={stack.StackName}
					onClose={() => setOpen(false)}
					onSaved={() => {
						setOpen(false);
						onDeleted();
					}}
					plan={async () => {
						if (stack.EnableTerminationProtection)
							throw new Error(
								"Disable termination protection before deleting this stack.",
							);
						return [
							{
								label: "Delete stack",
								run: () =>
									cfnCall(scope, "DeleteStack", {
										StackName: stack.StackId,
										...(retaining && retain.length
											? { RetainResources: retain }
											: {}),
									}),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						{stack.EnableTerminationProtection && (
							<Alert type="error">
								Termination protection blocks deletion. Disable it using the
								stack protection action first.
							</Alert>
						)}
						<Alert type="warning">
							Deleting stack {stack.StackName} deletes the resources it created,
							except those with a Retain deletion policy.
						</Alert>
						{retaining && (
							<FormField
								label="Resources to retain"
								description="These resources stay in place and leave the stack. Retain the resources that failed to delete."
							>
								<Multiselect
									ariaLabel="Resources to retain"
									placeholder="Choose resources to retain"
									selectedOptions={options.filter((o) =>
										retain.includes(o.value),
									)}
									options={options}
									onChange={({ detail }) =>
										setRetain(detail.selectedOptions.map((o) => o.value ?? ""))
									}
								/>
							</FormField>
						)}
					</SpaceBetween>
				</ActionDialog>
			)}
		</>
	);
}
