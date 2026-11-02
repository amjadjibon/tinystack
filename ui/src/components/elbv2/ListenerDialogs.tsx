import { Alert, SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { Pick, Text } from "#/components/memorydb/Fields";
import {
	type Certificate,
	type ELBv2Scope,
	elbv2Call,
	type Listener,
	type LoadBalancer,
	type SslPolicy,
	type TargetGroup,
} from "#/lib/elbv2";
import { isSecureProtocol, listenerProtocols } from "#/lib/elbv2Helpers";
import {
	actionFormFrom,
	emptyActionForm,
	type ListenerForm,
	listenerInput,
} from "#/lib/elbv2RuleHelpers";
import { ActionFields } from "./ActionFields";

export function ListenerDialog({
	scope,
	balancer,
	listener,
	groups,
	policies,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	balancer: LoadBalancer;
	listener?: Listener;
	groups: TargetGroup[];
	policies: SslPolicy[];
	onClose: () => void;
	onSaved: () => void;
}) {
	const protocols = listenerProtocols(balancer.Type);
	const [form, setForm] = useState<ListenerForm>({
		protocol: listener?.Protocol ?? protocols[0],
		port: String(listener?.Port ?? 80),
		certificate: listener?.Certificates?.[0]?.CertificateArn ?? "",
		sslPolicy: listener?.SslPolicy ?? "",
		action: listener
			? actionFormFrom(listener.DefaultActions)
			: emptyActionForm,
	});
	const set = (patch: Partial<ListenerForm>) =>
		setForm((f) => ({ ...f, ...patch }));
	const secure = isSecureProtocol(form.protocol);
	const suitable = policies.filter(
		(p) =>
			!p.SupportedLoadBalancerTypes?.length ||
			p.SupportedLoadBalancerTypes.includes(balancer.Type ?? "application"),
	);
	return (
		<ActionDialog
			title={
				listener
					? `Edit listener ${listener.Protocol}:${listener.Port}`
					: "Create listener"
			}
			submit={listener ? "Save listener" : "Create listener"}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				const input = listenerInput(form);
				return listener
					? [
							{
								label: "ModifyListener",
								run: () =>
									elbv2Call(scope, "ModifyListener", {
										ListenerArn: listener.ListenerArn,
										...input,
									}),
							},
						]
					: [
							{
								label: "CreateListener",
								run: () =>
									elbv2Call(scope, "CreateListener", {
										LoadBalancerArn: balancer.LoadBalancerArn,
										...input,
									}),
							},
						];
			}}
		>
			<SpaceBetween size="m">
				<Pick
					label="Protocol"
					value={form.protocol}
					placeholder="Choose a protocol"
					options={protocols.map((value) => ({ value }))}
					onChange={(protocol) => set({ protocol })}
				/>
				<Text
					label="Port"
					value={form.port}
					onChange={(port) => set({ port })}
				/>
				{secure && (
					<>
						<Text
							label="Certificate ARN"
							value={form.certificate}
							description="The ARN of an ACM certificate in this Region."
							onChange={(certificate) => set({ certificate })}
						/>
						<Pick
							label="SSL policy"
							value={form.sslPolicy}
							placeholder="Choose an SSL policy"
							options={suitable.map((p) => ({ value: p.Name ?? "" }))}
							onChange={(sslPolicy) => set({ sslPolicy })}
						/>
					</>
				)}
				<ActionFields
					form={form.action}
					groups={groups}
					onChange={(patch) => set({ action: { ...form.action, ...patch } })}
				/>
			</SpaceBetween>
		</ActionDialog>
	);
}

export function DeleteListenerDialog({
	scope,
	listener,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	listener: Listener;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Delete listener"
			submit="Delete listener"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "DeleteListener",
					run: () =>
						elbv2Call(scope, "DeleteListener", {
							ListenerArn: listener.ListenerArn,
						}),
				},
			]}
		>
			<Alert type="warning">
				Deleting the {listener.Protocol}:{listener.Port} listener also removes
				its rules.
			</Alert>
		</ActionDialog>
	);
}

export function AddCertificateDialog({
	scope,
	listener,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	listener: Listener;
	onClose: () => void;
	onSaved: () => void;
}) {
	const [arn, setArn] = useState("");
	return (
		<ActionDialog
			title="Add certificate"
			submit="Add certificate"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				if (!arn.trim()) throw new Error("Enter a certificate ARN.");
				return [
					{
						label: "AddListenerCertificates",
						run: () =>
							elbv2Call(scope, "AddListenerCertificates", {
								ListenerArn: listener.ListenerArn,
								Certificates: [{ CertificateArn: arn.trim() }],
							}),
					},
				];
			}}
		>
			<Text
				label="Certificate ARN"
				value={arn}
				description="The ARN of an ACM certificate in this Region."
				onChange={setArn}
			/>
		</ActionDialog>
	);
}

export function RemoveCertificateDialog({
	scope,
	listener,
	certificate,
	onClose,
	onSaved,
}: {
	scope: ELBv2Scope;
	listener: Listener;
	certificate: Certificate;
	onClose: () => void;
	onSaved: () => void;
}) {
	return (
		<ActionDialog
			title="Remove certificate"
			submit="Remove certificate"
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => [
				{
					label: "RemoveListenerCertificates",
					run: () =>
						elbv2Call(scope, "RemoveListenerCertificates", {
							ListenerArn: listener.ListenerArn,
							Certificates: [{ CertificateArn: certificate.CertificateArn }],
						}),
				},
			]}
		>
			<Alert type="warning">
				Remove {certificate.CertificateArn} from the listener. The default
				certificate cannot be removed.
			</Alert>
		</ActionDialog>
	);
}
