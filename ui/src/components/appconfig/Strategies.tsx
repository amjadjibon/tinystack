import {
	Alert,
	Box,
	Button,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Copy } from "#/components/efs/Shared";
import { type AppConfigScope, listStrategies } from "#/lib/appconfig";
import {
	type DeploymentStrategy,
	growthText,
	isPredefined,
	minutesText,
} from "#/lib/appconfigHelpers";
import { usePoll } from "#/lib/usePoll";
import { DeleteDialog } from "./Shared";
import { StrategyDialog } from "./StrategyDialogs";

type Dialog =
	| { kind: "create" }
	| { kind: "edit"; strategy: DeploymentStrategy }
	| { kind: "delete"; strategy: DeploymentStrategy };

export function Strategies({
	scope,
	enabled,
	onChanged,
}: {
	scope: AppConfigScope;
	enabled: boolean;
	onChanged: () => void;
}) {
	const poll = usePoll(
		() => (enabled ? listStrategies(scope) : Promise.resolve([])),
		0,
	);
	const [dialog, setDialog] = useState<Dialog>();
	const changed = () => {
		setDialog(undefined);
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="m">
			{poll.error && <Alert type="error">{poll.error}</Alert>}
			<Rows<DeploymentStrategy>
				title="Deployment strategies"
				emptyText="Choose Create strategy to add one."
				items={poll.data ?? []}
				loading={poll.loading}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Button
							variant="primary"
							disabled={!enabled}
							onClick={() => setDialog({ kind: "create" })}
						>
							Create strategy
						</Button>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Name", cell: (s) => s.Name ?? s.Id },
					{
						id: "id",
						header: "Strategy ID",
						// An AWS strategy's ID is its name.
						cell: (s) =>
							isPredefined(s) ? "-" : <Copy text={s.Id} label="ID" />,
					},
					{ id: "growth", header: "Growth", cell: (s) => growthText(s) },
					{
						id: "duration",
						header: "Deployment time",
						cell: (s) => minutesText(s.DeploymentDurationInMinutes),
					},
					{
						id: "bake",
						header: "Bake time",
						cell: (s) => minutesText(s.FinalBakeTimeInMinutes),
					},
					{
						id: "description",
						header: "Description",
						cell: (s) => s.Description || "-",
					},
					{
						id: "actions",
						header: "Actions",
						minWidth: 190,
						cell: (s) =>
							isPredefined(s) ? (
								<Box color="text-status-inactive">AWS predefined</Box>
							) : (
								<Buttons>
									<Action
										disabled={!enabled}
										onClick={() => setDialog({ kind: "edit", strategy: s })}
									>
										Edit
									</Action>
									<Action
										disabled={!enabled}
										onClick={() => setDialog({ kind: "delete", strategy: s })}
									>
										Delete
									</Action>
								</Buttons>
							),
					},
				]}
			/>
			{dialog?.kind === "create" && (
				<StrategyDialog
					scope={scope}
					onClose={() => setDialog(undefined)}
					onSaved={changed}
				/>
			)}
			{dialog?.kind === "edit" && (
				<StrategyDialog
					scope={scope}
					strategy={dialog.strategy}
					onClose={() => setDialog(undefined)}
					onSaved={changed}
				/>
			)}
			{dialog?.kind === "delete" && (
				<DeleteDialog
					scope={scope}
					title="Delete deployment strategy"
					noun="strategy"
					confirmation={dialog.strategy.Name ?? dialog.strategy.Id}
					operation="DeleteDeploymentStrategy"
					input={{ DeploymentStrategyId: dialog.strategy.Id }}
					warning={`Delete ${dialog.strategy.Name ?? dialog.strategy.Id} permanently. Deployments that used it keep the settings they recorded.`}
					onClose={() => setDialog(undefined)}
					onSaved={changed}
				/>
			)}
		</SpaceBetween>
	);
}
