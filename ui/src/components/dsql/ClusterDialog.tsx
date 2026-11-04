import { Alert, Checkbox, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Choice, Field } from "#/components/containers/Controls";
import { dsqlCall } from "#/lib/dsql";
import {
	type Cluster,
	createInput,
	type DSQLScope,
	needsPeer,
	updateInput,
	witnessRegions,
} from "#/lib/dsqlHelpers";

export function ClusterDialog({
	scope,
	cluster,
	onClose,
	onSaved,
}: {
	scope: DSQLScope;
	cluster?: Cluster;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [f, setF] = useState({
		protection: cluster?.DeletionProtectionEnabled ?? true,
		tags: "{}",
		multi: false,
		witness: "",
		peer: "",
	});
	const set = (p: Partial<typeof f>) => setF((v) => ({ ...v, ...p }));
	const witnesses = witnessRegions(scope.region);
	const op = cluster ? "UpdateCluster" : "CreateCluster";
	// Cloudscape's SpaceBetween does not see through fragments with React 19, so each
	// group of fields below is a SpaceBetween of its own.
	return (
		<ActionDialog
			title={cluster ? "Edit cluster settings" : "Create cluster"}
			submit={cluster ? "Save changes" : "Create cluster"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async (token) => {
				const input = cluster
					? updateInput(cluster, f)
					: createInput(f, scope.region);
				input.clientToken = token;
				return [{ label: op, run: () => dsqlCall(scope, op, input) }];
			}}
		>
			<SpaceBetween size="m">
				<Checkbox
					checked={f.protection}
					description="Blocks deleting the cluster until you turn this off. AWS turns it on for new clusters."
					onChange={({ detail }) => set({ protection: detail.checked })}
				>
					Deletion protection
				</Checkbox>
				{!cluster && (
					<SpaceBetween size="m">
						<Field
							label="Tags (JSON string map)"
							value={f.tags}
							onChange={(tags) => set({ tags })}
						/>
						<Checkbox
							checked={f.multi}
							disabled={!witnesses.length}
							description={
								witnesses.length
									? "Pairs this cluster with one in another Region of the same set, and a third Region as witness."
									: `Multi-Region clusters need a Region in the Americas, Asia Pacific or Europe set. ${scope.region} is in none of them.`
							}
							onChange={({ detail }) => set({ multi: detail.checked })}
						>
							Multi-Region cluster
						</Checkbox>
						{f.multi && (
							<SpaceBetween size="m">
								<Choice
									label="Witness Region"
									placeholder="Choose a Region"
									value={f.witness}
									options={witnesses}
									onChange={(witness) => set({ witness })}
								/>
								<Field
									label="Peer cluster ARN (optional)"
									value={f.peer}
									onChange={(peer) => set({ peer })}
								/>
								<Alert>
									Create the first cluster with a witness Region. Create the
									second in another Region with the first cluster's ARN as its
									peer. Then edit the first cluster and enter the second
									cluster's ARN. Both become ACTIVE once each lists the other.
								</Alert>
							</SpaceBetween>
						)}
					</SpaceBetween>
				)}
				{!!cluster && needsPeer(cluster) && (
					<SpaceBetween size="m">
						<Field
							label="Peer cluster ARN"
							value={f.peer}
							onChange={(peer) => set({ peer })}
						/>
						<Alert>
							Enter the ARN of the cluster created as this one's peer. Both
							become ACTIVE once each lists the other.
						</Alert>
					</SpaceBetween>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
