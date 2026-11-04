import {
	Box,
	FormField,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type ACMScope, acmCall } from "#/lib/acm";
import { importInput } from "#/lib/acmHelpers";

// ImportDialog imports a certificate, or reimports over arn when it is set.
export function ImportDialog({
	scope,
	arn = "",
	onClose,
	onSaved,
}: {
	scope: ACMScope;
	arn?: string;
	onClose: () => void;
	onSaved: (arn: string) => void;
}) {
	const [certificate, setCertificate] = useState(""),
		[privateKey, setPrivateKey] = useState(""),
		[chain, setChain] = useState(""),
		[tags, setTags] = useState("");
	const imported = useRef("");
	return (
		<ActionDialog
			title={arn ? "Reimport certificate" : "Import certificate"}
			submit={arn ? "Reimport certificate" : "Import certificate"}
			onClose={onClose}
			onSaved={() => onSaved(imported.current)}
			plan={async () => {
				const input = importInput({
					arn,
					certificate,
					privateKey,
					chain,
					tags,
				});
				return [
					{
						label: arn ? "Reimport certificate" : "Import certificate",
						run: async () => {
							const out = await acmCall<{ CertificateArn: string }>(
								scope,
								"ImportCertificate",
								input,
							);
							imported.current = out.CertificateArn;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				{!!arn && (
					<Box color="text-body-secondary">
						Replaces the certificate body, chain and private key of {arn}. The
						ARN and tags stay the same.
					</Box>
				)}
				<FormField label="Certificate body (PEM)">
					<Textarea
						ariaLabel="Certificate body"
						rows={8}
						placeholder="-----BEGIN CERTIFICATE-----"
						value={certificate}
						onChange={({ detail }) => setCertificate(detail.value)}
					/>
				</FormField>
				<FormField
					label="Private key (PEM)"
					description="The key is sent to TinyStack and is never shown again."
				>
					<Textarea
						ariaLabel="Certificate private key"
						rows={8}
						placeholder="-----BEGIN PRIVATE KEY-----"
						value={privateKey}
						onChange={({ detail }) => setPrivateKey(detail.value)}
					/>
				</FormField>
				<FormField label="Certificate chain (PEM, optional)">
					<Textarea
						ariaLabel="Certificate chain"
						rows={6}
						value={chain}
						onChange={({ detail }) => setChain(detail.value)}
					/>
				</FormField>
				{!arn && (
					<FormField
						label="Tags (JSON)"
						description='Optional object of string values, for example {"team": "web"}.'
					>
						<Textarea
							ariaLabel="Certificate tags"
							rows={3}
							value={tags}
							onChange={({ detail }) => setTags(detail.value)}
						/>
					</FormField>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
