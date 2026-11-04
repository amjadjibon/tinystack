import {
	Alert,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { Pick, Text } from "#/components/memorydb/Fields";
import { Facts, ResourceTags } from "#/components/sesv2/Facts";
import { unixTime } from "#/lib/format";
import {
	getIdentity,
	type IdentityInfo,
	resourceArn,
	type SESScope,
	sesv2Call,
} from "#/lib/sesv2";
import {
	dkimSummary,
	identityKind,
	validateIdentity,
} from "#/lib/sesv2Helpers";
import { usePoll } from "#/lib/usePoll";

const none = "(none)";

export function SESIdentities({
	scope,
	enabled,
	identities,
	configSets,
	loading,
	onChanged,
}: {
	scope: SESScope;
	enabled: boolean;
	identities: IdentityInfo[];
	configSets: string[];
	loading: boolean;
	onChanged: () => void;
}) {
	const [dialog, setDialog] = useState<"" | "create" | "delete">(""),
		[name, setName] = useState(""),
		[set, setSet] = useState(none),
		[selected, setSelected] = useState("");
	const open = (kind: "create" | "delete", identity = "") => {
		setName(identity);
		setSet(none);
		setDialog(kind);
	};
	return (
		<SpaceBetween size="m">
			<Rows
				title="Identities"
				emptyText="Choose Create identity to verify an email address or a domain."
				items={identities}
				loading={loading}
				actions={
					<Buttons>
						<Action onClick={onChanged}>Refresh</Action>
						<Action disabled={!enabled} onClick={() => open("create")}>
							Create identity
						</Action>
					</Buttons>
				}
				columns={[
					{ id: "name", header: "Identity", cell: (i) => i.IdentityName },
					{
						id: "type",
						header: "Type",
						cell: (i) =>
							i.IdentityType === "DOMAIN" ? "Domain" : "Email address",
					},
					{
						id: "status",
						header: "Verification",
						cell: (i) => (
							<StatusIndicator
								type={
									i.VerificationStatus === "SUCCESS" ? "success" : "pending"
								}
							>
								{i.VerificationStatus ?? "-"}
							</StatusIndicator>
						),
					},
					{
						id: "sending",
						header: "Sending",
						cell: (i) => (i.SendingEnabled ? "Enabled" : "Disabled"),
					},
					{
						id: "actions",
						header: "Actions",
						cell: (i) => (
							<Buttons>
								<Action onClick={() => setSelected(i.IdentityName ?? "")}>
									View
								</Action>
								<Action
									disabled={!enabled}
									onClick={() => open("delete", i.IdentityName ?? "")}
								>
									Delete
								</Action>
							</Buttons>
						),
					},
				]}
			/>
			{!!selected && (
				<IdentityDetails
					key={selected}
					scope={scope}
					name={selected}
					configSets={configSets}
					enabled={enabled}
				/>
			)}
			{dialog === "create" && (
				<ActionDialog
					title="Create identity"
					submit="Create identity"
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						setSelected(name);
						onChanged();
					}}
					plan={async () => {
						const problem = validateIdentity(name);
						if (problem) throw new Error(problem);
						return [
							{
								label: "CreateEmailIdentity",
								run: () =>
									sesv2Call(scope, "CreateEmailIdentity", {
										EmailIdentity: name,
										...(set !== none ? { ConfigurationSetName: set } : {}),
									}),
							},
						];
					}}
				>
					<SpaceBetween size="m">
						<Text
							label="Email address or domain"
							value={name}
							onChange={setName}
							description="TinyStack verifies identities immediately. A domain also gets DKIM tokens."
							constraint="For example sender@example.com or example.com."
						/>
						<Pick
							label="Default configuration set (optional)"
							value={set}
							placeholder="Choose a configuration set"
							options={[
								{ value: none },
								...configSets.map((value) => ({ value })),
							]}
							onChange={setSet}
						/>
					</SpaceBetween>
				</ActionDialog>
			)}
			{dialog === "delete" && (
				<ActionDialog
					title="Delete identity"
					submit="Delete identity"
					confirmation={name}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						if (selected === name) setSelected("");
						onChanged();
					}}
					plan={async () => [
						{
							label: "DeleteEmailIdentity",
							run: () =>
								sesv2Call(scope, "DeleteEmailIdentity", {
									EmailIdentity: name,
								}),
						},
					]}
				>
					<Alert type="warning">
						Messages can no longer be sent from {name}.
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}

function IdentityDetails({
	scope,
	name,
	enabled,
	configSets,
}: {
	scope: SESScope;
	name: string;
	enabled: boolean;
	configSets: string[];
}) {
	const [editing, setEditing] = useState(false),
		[set, setSet] = useState(none);
	const poll = usePoll(() => getIdentity(scope, name), 0),
		d = poll.data;
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			<Facts
				title={name}
				actions={
					<Buttons>
						<Action onClick={poll.reload}>Refresh</Action>
						<Action
							disabled={!enabled || !d}
							onClick={() => {
								setSet(d?.ConfigurationSetName || none);
								setEditing(true);
							}}
						>
							Edit default configuration set
						</Action>
					</Buttons>
				}
				facts={[
					[
						"Type",
						identityKind(name) === "domain" ? "Domain" : "Email address",
					],
					["Verification status", d?.VerificationStatus],
					[
						"Verified for sending",
						d ? String(!!d.VerifiedForSendingStatus) : "",
					],
					[
						"Feedback forwarding",
						d ? (d.FeedbackForwardingStatus ? "Enabled" : "Disabled") : "",
					],
					["Configuration set", d?.ConfigurationSetName],
					["DKIM", dkimSummary(d?.DkimAttributes)],
					["DKIM origin", d?.DkimAttributes?.SigningAttributesOrigin],
					["Current key length", d?.DkimAttributes?.CurrentSigningKeyLength],
					[
						"Key generated",
						unixTime(d?.DkimAttributes?.LastKeyGenerationTimestamp),
					],
				]}
			/>
			{!!d?.DkimAttributes?.Tokens?.length && (
				<CodeBlock text={d.DkimAttributes.Tokens.join("\n")} />
			)}
			{editing && (
				<ActionDialog
					title="Edit default configuration set"
					onClose={() => setEditing(false)}
					onSaved={() => {
						setEditing(false);
						poll.reload();
					}}
					plan={async () => [
						{
							label: "PutEmailIdentityConfigurationSetAttributes",
							run: () =>
								sesv2Call(scope, "PutEmailIdentityConfigurationSetAttributes", {
									EmailIdentity: name,
									...(set !== none ? { ConfigurationSetName: set } : {}),
								}),
						},
					]}
				>
					<Pick
						label="Default configuration set"
						value={set}
						placeholder="Choose a configuration set"
						options={[
							{ value: none },
							...configSets.map((value) => ({ value })),
						]}
						onChange={setSet}
					/>
				</ActionDialog>
			)}
			<ResourceTags
				scope={scope}
				arn={resourceArn(scope, "identity", name)}
				enabled={enabled}
			/>
		</SpaceBetween>
	);
}
