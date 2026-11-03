import {
	Alert,
	Container,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { Action, Rows } from "#/components/containers/Controls";
import {
	type Certificate,
	type ELBv2Scope,
	type Listener,
	listListenerCertificates,
	type TargetGroup,
} from "#/lib/elbv2";
import { isSecureProtocol } from "#/lib/elbv2Helpers";
import { usePoll } from "#/lib/usePoll";
import {
	AddCertificateDialog,
	RemoveCertificateDialog,
} from "./ListenerDialogs";
import { Rules } from "./Rules";
import { AttributesDialog } from "./Shared";

export function ListenerDetails({
	scope,
	listener,
	groups,
	enabled,
	onChanged,
}: {
	scope: ELBv2Scope;
	listener: Listener;
	groups: TargetGroup[];
	enabled: boolean;
	onChanged: () => void;
}) {
	const secure = isSecureProtocol(listener.Protocol ?? "");
	const certs = usePoll(
		async () =>
			secure ? listListenerCertificates(scope, listener.ListenerArn) : [],
		0,
	);
	const [dialog, setDialog] = useState<"" | "attributes" | "add" | "remove">(
		"",
	);
	const [certificate, setCertificate] = useState<Certificate>();
	const saved = () => {
		setDialog("");
		certs.reload();
		onChanged();
	};
	return (
		<Container
			header={
				<Header
					variant="h2"
					description={listener.ListenerArn}
					actions={
						<Action disabled={!enabled} onClick={() => setDialog("attributes")}>
							Edit attributes
						</Action>
					}
				>
					Listener {listener.Protocol}:{listener.Port}
				</Header>
			}
		>
			<SpaceBetween size="l">
				{secure && (
					<SpaceBetween size="l">
						{certs.error && <Alert type="error">{certs.error}</Alert>}
						<Rows
							title="Certificates"
							emptyText="Choose Add certificate to serve another certificate."
							items={certs.data ?? []}
							loading={certs.loading}
							actions={
								<Action disabled={!enabled} onClick={() => setDialog("add")}>
									Add certificate
								</Action>
							}
							columns={[
								{
									id: "arn",
									header: "Certificate ARN",
									cell: (c) => c.CertificateArn,
								},
								{
									id: "default",
									header: "Default",
									cell: (c) => (c.IsDefault ? "Yes" : "No"),
								},
								{
									id: "remove",
									header: "Actions",
									cell: (c) => (
										<Action
											disabled={!enabled || c.IsDefault}
											onClick={() => {
												setCertificate(c);
												setDialog("remove");
											}}
										>
											Remove
										</Action>
									),
								},
							]}
						/>
					</SpaceBetween>
				)}
				<Rules
					scope={scope}
					listener={listener}
					groups={groups}
					enabled={enabled}
				/>
			</SpaceBetween>
			{dialog === "attributes" && (
				<AttributesDialog
					scope={scope}
					title={`Attributes of ${listener.Protocol}:${listener.Port}`}
					arn={listener.ListenerArn}
					idKey="ListenerArn"
					describe="DescribeListenerAttributes"
					modify="ModifyListenerAttributes"
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "add" && (
				<AddCertificateDialog
					scope={scope}
					listener={listener}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "remove" && certificate && (
				<RemoveCertificateDialog
					scope={scope}
					listener={listener}
					certificate={certificate}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
		</Container>
	);
}
