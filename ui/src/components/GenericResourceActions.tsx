import {
	Button,
	CopyToClipboard,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { api, type Resource } from "#/lib/api";
import { deleteInput, resourceTagPlan } from "#/lib/genericResources";
import { useScope } from "#/lib/scope";
import { ActionDialog } from "./ActionDialog";
import { ConsoleTags } from "./ConsoleTags";

export function GenericResourceActions({
	resource,
	onChanged,
}: {
	resource: Resource;
	onChanged: () => void;
}) {
	const { capabilities, canWrite, region } = useScope();
	const [deleting, setDeleting] = useState(false);
	const action =
		capabilities.data?.resource_deletes[`${resource.service}/${resource.type}`];
	return (
		<SpaceBetween direction="horizontal" size="xs">
			{!!resource.arn && (
				<CopyToClipboard
					textToCopy={resource.arn}
					copyButtonText="Copy ARN"
					copySuccessText="ARN copied"
					copyErrorText="ARN failed to copy"
				/>
			)}
			{!!resource.arn &&
				capabilities.data?.taggable_services.includes(resource.service) && (
					<ConsoleTags
						compact
						tags={resource.tags ?? {}}
						enabled={canWrite}
						plan={(next) =>
							resourceTagPlan(
								api.action,
								resource,
								resource.region && resource.region !== "global"
									? resource.region
									: region,
								next,
							)
						}
						onChanged={onChanged}
					/>
				)}
			{action && (
				<Button disabled={!canWrite} onClick={() => setDeleting(true)}>
					Delete
				</Button>
			)}
			{deleting && action && (
				<ActionDialog
					title={`Delete ${resource.name}`}
					submit="Delete resource"
					confirmation={resource.name}
					onClose={() => setDeleting(false)}
					onSaved={() => {
						setDeleting(false);
						onChanged();
					}}
					plan={async () => [
						{
							label: action.operation,
							run: () =>
								api.action(
									action.service,
									action.operation,
									resource.account,
									deleteInput(resource, action),
									resource.region && resource.region !== "global"
										? resource.region
										: region,
								),
						},
					]}
				>
					<p>
						Delete {resource.type} <strong>{resource.name}</strong>?
					</p>
					<p>ARN: {resource.arn}</p>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
