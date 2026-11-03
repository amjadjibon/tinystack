import {
	Alert,
	FormField,
	Select,
	SpaceBetween,
	Toggle,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type OpenSearchScope, opensearchCall } from "#/lib/opensearch";
import { type DomainStatus, upgradeTargets } from "#/lib/opensearchHelpers";
import { usePoll } from "#/lib/usePoll";

export function UpgradeDialog({
	scope,
	domain,
	onClose,
	onSaved,
}: {
	scope: OpenSearchScope;
	domain: DomainStatus;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [target, setTarget] = useState("");
	const [checkOnly, setCheckOnly] = useState(true);
	const compatible = usePoll(
		() =>
			opensearchCall<{
				CompatibleVersions?: {
					SourceVersion?: string;
					TargetVersions?: string[];
				}[];
			}>(scope, "GetCompatibleVersions", { DomainName: domain.DomainName }),
		0,
	);
	const targets = upgradeTargets(
		compatible.data?.CompatibleVersions ?? [],
		domain.EngineVersion,
	);
	return (
		<ActionDialog
			title="Upgrade engine"
			submit={checkOnly ? "Run upgrade check" : "Upgrade"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!targets.includes(target))
					throw new Error("Choose a compatible target version.");
				return [
					{
						label: checkOnly ? "Check upgrade" : "UpgradeDomain",
						run: () =>
							opensearchCall(scope, "UpgradeDomain", {
								DomainName: domain.DomainName,
								TargetVersion: target,
								PerformCheckOnly: checkOnly,
							}),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!!compatible.error && <Alert type="error">{compatible.error}</Alert>}
				<FormField label={`Compatible versions for ${domain.EngineVersion}`}>
					<Select
						selectedOption={target ? { value: target } : null}
						options={targets.map((value) => ({ value }))}
						onChange={({ detail }) =>
							setTarget(detail.selectedOption.value ?? "")
						}
					/>
				</FormField>
				<Toggle
					checked={checkOnly}
					onChange={({ detail }) => setCheckOnly(detail.checked)}
				>
					Perform compatibility check only
				</Toggle>
				<Alert type="info">
					Checks and upgrades appear in upgrade history. A check leaves the
					engine version unchanged.
				</Alert>
			</SpaceBetween>
		</ActionDialog>
	);
}
