import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice } from "#/components/acm/Fields";
import { Text } from "#/components/memorydb/Fields";
import { type AppConfigScope, appconfigCall } from "#/lib/appconfig";
import {
	createStrategyInput,
	type DeploymentStrategy,
	emptyStrategyForm,
	formFromStrategy,
	growthTypes,
	type StrategyForm,
	updateStrategyInput,
} from "#/lib/appconfigHelpers";

// StrategyDialog creates a deployment strategy, or edits one of the strategies you made.
export function StrategyDialog({
	scope,
	strategy,
	onClose,
	onSaved,
}: {
	scope: AppConfigScope;
	// The strategy being edited, none when a new one is created.
	strategy?: DeploymentStrategy;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [form, setForm] = useState(() =>
		strategy ? formFromStrategy(strategy) : emptyStrategyForm(),
	);
	const set = (patch: Partial<StrategyForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	const operation = strategy
		? "UpdateDeploymentStrategy"
		: "CreateDeploymentStrategy";
	return (
		<ActionDialog
			title={
				strategy
					? `Edit ${strategy.Name ?? strategy.Id}`
					: "Create deployment strategy"
			}
			submit={strategy ? "Save strategy" : "Create strategy"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = strategy
					? updateStrategyInput(strategy, form)
					: createStrategyInput(form);
				return [
					{
						label: operation,
						run: () => appconfigCall(scope, operation, input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Alert type="info">
					TinyStack completes a deployment at once, whatever its strategy. The
					settings are recorded with each deployment that uses the strategy.
				</Alert>
				<Text
					label="Name"
					value={form.name}
					disabled={!!strategy}
					onChange={(name) => set({ name })}
					constraint={
						strategy
							? "A strategy cannot be renamed."
							: "1 to 64 characters, not starting with AppConfig."
					}
				/>
				<Text
					label="Description"
					value={form.description}
					onChange={(description) => set({ description })}
					constraint="Optional, up to 1024 characters."
				/>
				<Choice
					label="Growth type"
					value={form.growth}
					options={growthTypes}
					onChange={(growth) => set({ growth })}
				/>
				<Text
					label="Growth factor (%)"
					value={form.factor}
					onChange={(factor) => set({ factor })}
					constraint="A number from 1 to 100."
				/>
				<Text
					label="Deployment time (minutes)"
					value={form.duration}
					onChange={(duration) => set({ duration })}
					constraint="A whole number from 0 to 1440."
				/>
				<Text
					label="Bake time (minutes)"
					value={form.bake}
					onChange={(bake) => set({ bake })}
					constraint="A whole number from 0 to 1440."
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}
