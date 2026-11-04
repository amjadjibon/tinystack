import {
	Alert,
	Box,
	Button,
	ColumnLayout,
	Container,
	CopyToClipboard,
	FormField,
	Header,
	Input,
	SpaceBetween,
	StatusIndicator,
	Table,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { CodeBlock } from "#/components/CodeBlock";
import { ConsoleTags } from "#/components/ConsoleTags";
import {
	type ACMScope,
	acmCall,
	type CertificateDetail as Detail,
} from "#/lib/acm";
import {
	certificateType,
	maskKey,
	passphraseError,
	pemFileName,
	utf8Base64,
} from "#/lib/acmHelpers";
import { unixTime } from "#/lib/format";
import { tagList } from "#/lib/tags";
import { usePoll } from "#/lib/usePoll";
import { Choice } from "./Fields";
import { ImportDialog } from "./ImportDialog";

type Exported = {
	Certificate?: string;
	CertificateChain?: string;
	PrivateKey?: string;
};

function download(name: string, text: string) {
	const url = URL.createObjectURL(
		new Blob([text], { type: "application/x-pem-file" }),
	);
	const link = document.createElement("a");
	link.href = url;
	link.download = name;
	link.click();
	URL.revokeObjectURL(url);
}

function Field({
	label,
	children,
}: {
	label: string;
	children: React.ReactNode;
}) {
	return (
		<div>
			<Box variant="awsui-key-label">{label}</Box>
			<div>{children}</div>
		</div>
	);
}

function PemSection({
	title,
	text,
	file,
}: {
	title: string;
	text: string;
	file: string;
}) {
	return (
		<SpaceBetween size="xs">
			<Header
				variant="h3"
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<CopyToClipboard
							variant="button"
							textToCopy={text}
							copyButtonText="Copy"
							copySuccessText={`${title} copied`}
							copyErrorText={`${title} failed to copy`}
						/>
						<Button iconName="download" onClick={() => download(file, text)}>
							Download
						</Button>
					</SpaceBetween>
				}
			>
				{title}
			</Header>
			<CodeBlock text={text} />
		</SpaceBetween>
	);
}

function ExportDialog({
	scope,
	arn,
	onClose,
	onExported,
}: {
	scope: ACMScope;
	arn: string;
	onClose: () => void;
	onExported: (out: Exported) => void;
}) {
	const [passphrase, setPassphrase] = useState("");
	const out = useRef<Exported>({});
	return (
		<ActionDialog
			title="Export certificate"
			submit="Export certificate"
			onClose={onClose}
			onSaved={() => onExported(out.current)}
			plan={async () => {
				const problem = passphraseError(passphrase);
				if (problem) throw new Error(problem);
				return [
					{
						label: "Export certificate",
						run: async () => {
							out.current = await acmCall<Exported>(
								scope,
								"ExportCertificate",
								{
									CertificateArn: arn,
									Passphrase: utf8Base64(passphrase),
								},
							);
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Box color="text-body-secondary">
					Only certificates that TinyStack issued with Allow export enabled can
					be exported. The private key is encrypted with the passphrase.
				</Box>
				<FormField label="Passphrase" description="4 to 128 bytes.">
					<Input
						ariaLabel="Export passphrase"
						type="password"
						value={passphrase}
						onChange={({ detail }) => setPassphrase(detail.value)}
					/>
				</FormField>
			</SpaceBetween>
		</ActionDialog>
	);
}

function ExportResult({ out, domain }: { out: Exported; domain: string }) {
	const [reveal, setReveal] = useState(false);
	const key = out.PrivateKey ?? "";
	return (
		<Container
			header={
				<Header variant="h2">
					Exported certificate (kept only until you leave this page)
				</Header>
			}
		>
			<SpaceBetween size="m">
				{!!out.Certificate && (
					<PemSection
						title="Exported certificate"
						text={out.Certificate}
						file={pemFileName(domain, "")}
					/>
				)}
				{!!out.CertificateChain && (
					<PemSection
						title="Exported chain"
						text={out.CertificateChain}
						file={pemFileName(domain, "-chain")}
					/>
				)}
				{!!key && (
					<SpaceBetween size="xs">
						<Header
							variant="h3"
							actions={
								<SpaceBetween direction="horizontal" size="xs">
									<Button onClick={() => setReveal(!reveal)}>
										{reveal ? "Hide private key" : "Reveal private key"}
									</Button>
									<CopyToClipboard
										variant="button"
										textToCopy={key}
										copyButtonText="Copy"
										copySuccessText="Private key copied"
										copyErrorText="Private key failed to copy"
									/>
									<Button
										iconName="download"
										onClick={() => download(pemFileName(domain, "-key"), key)}
									>
										Download
									</Button>
								</SpaceBetween>
							}
						>
							Encrypted private key
						</Header>
						<CodeBlock text={maskKey(key, reveal)} />
					</SpaceBetween>
				)}
			</SpaceBetween>
		</Container>
	);
}

export function CertificateDetails({
	scope,
	enabled,
	onChanged,
	onDeleted,
}: {
	scope: ACMScope;
	enabled: boolean;
	onChanged: () => void;
	onDeleted: () => void;
}) {
	const arn = scope.certificate ?? "";
	const [dialog, setDialog] = useState<
		"" | "export" | "options" | "reimport" | "delete"
	>("");
	const [exported, setExported] = useState<Exported>();
	const [preference, setPreference] = useState("ENABLED");
	const [pending, setPending] = useState(false);
	const poll = usePoll(
		async () => {
			if (!enabled) return undefined;
			const { Certificate } = await acmCall<{ Certificate: Detail }>(
				scope,
				"DescribeCertificate",
				{ CertificateArn: arn },
			);
			setPending(Certificate.Status === "PENDING_VALIDATION");
			const [tags, pem] = await Promise.all([
				acmCall<{ Tags?: { Key: string; Value?: string }[] }>(
					scope,
					"ListTagsForCertificate",
					{ CertificateArn: arn },
				),
				Certificate.Status === "ISSUED" || Certificate.Status === "EXPIRED"
					? acmCall<{ Certificate?: string; CertificateChain?: string }>(
							scope,
							"GetCertificate",
							{ CertificateArn: arn },
						)
					: Promise.resolve(undefined),
			]);
			return {
				cert: Certificate,
				pem,
				tags: Object.fromEntries(
					(tags.Tags ?? []).map((t) => [t.Key, t.Value ?? ""]),
				),
			};
		},
		pending ? 5000 : 0,
	);
	const cert = poll.data?.cert;
	const tags = poll.data?.tags ?? {};
	const changed = () => {
		poll.reload();
		onChanged();
	};
	return (
		<SpaceBetween size="l">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{!enabled && (
				<Alert type="info">
					Connect a console session to load certificate details.
				</Alert>
			)}
			<Header
				variant="h1"
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button onClick={poll.reload}>Refresh</Button>
						<Button
							disabled={
								!enabled ||
								cert?.Type !== "AMAZON_ISSUED" ||
								cert.Options?.Export !== "ENABLED"
							}
							onClick={() => setDialog("export")}
						>
							Export certificate
						</Button>
						<Button
							disabled={!enabled || !cert}
							onClick={() => {
								setPreference(
									cert?.Options?.CertificateTransparencyLoggingPreference ??
										"ENABLED",
								);
								setDialog("options");
							}}
						>
							Update options
						</Button>
						<Button
							disabled={!enabled || cert?.Type !== "IMPORTED"}
							onClick={() => setDialog("reimport")}
						>
							Reimport certificate
						</Button>
						<Button
							disabled={!enabled || !cert}
							onClick={() => setDialog("delete")}
						>
							Delete certificate
						</Button>
					</SpaceBetween>
				}
			>
				{cert?.DomainName ?? arn}
			</Header>
			{cert && (
				<Container header={<Header variant="h2">Certificate</Header>}>
					<ColumnLayout columns={3} variant="text-grid">
						<Field label="ARN">
							<CopyToClipboard
								variant="inline"
								textToCopy={cert.CertificateArn}
								copySuccessText="ARN copied"
								copyErrorText="ARN failed to copy"
							/>
						</Field>
						<Field label="Status">
							<StatusIndicator
								type={
									cert.Status === "ISSUED"
										? "success"
										: cert.Status === "PENDING_VALIDATION"
											? "pending"
											: "error"
								}
							>
								{cert.Status}
							</StatusIndicator>
						</Field>
						<Field label="Type">{certificateType(cert.Type)}</Field>
						<Field label="Domain names">
							{cert.SubjectAlternativeNames?.join(", ") || "—"}
						</Field>
						<Field label="Key algorithm">{cert.KeyAlgorithm ?? "—"}</Field>
						<Field label="Signature algorithm">
							{cert.SignatureAlgorithm ?? "—"}
						</Field>
						<Field label="Renewal eligibility">
							{cert.RenewalEligibility ?? "—"}
						</Field>
						<Field label="Transparency logging">
							{cert.Options?.CertificateTransparencyLoggingPreference ?? "—"}
						</Field>
						<Field label="Export">{cert.Options?.Export ?? "—"}</Field>
						<Field label="Issued / imported at">
							{unixTime(cert.IssuedAt ?? cert.ImportedAt)}
						</Field>
						<Field label="Not before">{unixTime(cert.NotBefore)}</Field>
						<Field label="Not after">{unixTime(cert.NotAfter)}</Field>
						<Field label="Subject">{cert.Subject ?? "—"}</Field>
						<Field label="Issuer">{cert.Issuer ?? "—"}</Field>
						<Field label="Serial">{cert.Serial ?? "—"}</Field>
						<Field label="In use by">
							{cert.InUseBy?.length ? cert.InUseBy.join(", ") : "Not in use"}
						</Field>
						<Field label="Key usages">
							{cert.KeyUsages?.map((k) => k.Name).join(", ") || "—"}
						</Field>
						<Field label="Extended key usages">
							{cert.ExtendedKeyUsages?.map((k) => k.Name).join(", ") || "—"}
						</Field>
					</ColumnLayout>
				</Container>
			)}
			{cert && (
				<Table
					header={<Header variant="h2">Domain validation</Header>}
					items={cert.DomainValidationOptions ?? []}
					trackBy="DomainName"
					empty={
						<Box>
							Imported certificates have no domain validation. Choose Request
							certificate on the list to create one that does.
						</Box>
					}
					columnDefinitions={[
						{ id: "domain", header: "Domain", cell: (v) => v.DomainName },
						{
							id: "status",
							header: "Status",
							cell: (v) => v.ValidationStatus ?? "—",
						},
						{
							id: "method",
							header: "Method",
							cell: (v) => v.ValidationMethod ?? "—",
						},
						{
							id: "name",
							header: "CNAME name",
							cell: (v) =>
								v.ResourceRecord ? (
									<CopyToClipboard
										variant="inline"
										textToCopy={v.ResourceRecord.Name}
										copySuccessText="CNAME name copied"
										copyErrorText="CNAME name failed to copy"
									/>
								) : (
									(v.ValidationEmails?.join(", ") ?? "—")
								),
						},
						{
							id: "value",
							header: "CNAME value",
							cell: (v) =>
								v.ResourceRecord ? (
									<CopyToClipboard
										variant="inline"
										textToCopy={v.ResourceRecord.Value}
										copySuccessText="CNAME value copied"
										copyErrorText="CNAME value failed to copy"
									/>
								) : (
									"—"
								),
						},
					]}
				/>
			)}
			{!!poll.data?.pem?.Certificate && (
				<Container header={<Header variant="h2">Certificate PEM</Header>}>
					<SpaceBetween size="m">
						<PemSection
							title="Certificate body"
							text={poll.data.pem.Certificate}
							file={pemFileName(cert?.DomainName, "")}
						/>
						{!!poll.data.pem.CertificateChain && (
							<PemSection
								title="Certificate chain"
								text={poll.data.pem.CertificateChain}
								file={pemFileName(cert?.DomainName, "-chain")}
							/>
						)}
					</SpaceBetween>
				</Container>
			)}
			{exported && (
				<ExportResult out={exported} domain={cert?.DomainName ?? ""} />
			)}
			<ConsoleTags
				tags={tags}
				enabled={enabled && !!cert}
				onChanged={changed}
				plan={(next, removed) => [
					...(removed.length
						? [
								{
									label: "Remove tags",
									run: () =>
										acmCall(scope, "RemoveTagsFromCertificate", {
											CertificateArn: arn,
											Tags: removed.map((Key) => ({ Key })),
										}),
								},
							]
						: []),
					...(Object.keys(next).length
						? [
								{
									label: "Save tags",
									run: () =>
										acmCall(scope, "AddTagsToCertificate", {
											CertificateArn: arn,
											Tags: tagList(next),
										}),
								},
							]
						: []),
				]}
			/>
			{dialog === "export" && (
				<ExportDialog
					scope={scope}
					arn={arn}
					onClose={() => setDialog("")}
					onExported={(out) => {
						setDialog("");
						setExported(out);
						changed();
					}}
				/>
			)}
			{dialog === "options" && (
				<ActionDialog
					title="Update certificate options"
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						changed();
					}}
					plan={async () => [
						{
							label: "Update options",
							run: () =>
								acmCall(scope, "UpdateCertificateOptions", {
									CertificateArn: arn,
									Options: {
										CertificateTransparencyLoggingPreference: preference,
									},
								}),
						},
					]}
				>
					<Choice
						label="Transparency logging"
						value={preference}
						options={["ENABLED", "DISABLED"]}
						onChange={setPreference}
					/>
				</ActionDialog>
			)}
			{dialog === "reimport" && (
				<ImportDialog
					scope={scope}
					arn={arn}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						changed();
					}}
				/>
			)}
			{dialog === "delete" && cert && (
				<ActionDialog
					title="Delete certificate"
					submit="Delete certificate"
					confirmation={cert.DomainName}
					onClose={() => setDialog("")}
					onSaved={() => {
						setDialog("");
						onDeleted();
					}}
					plan={async () => [
						{
							label: "Delete certificate",
							run: () =>
								acmCall(scope, "DeleteCertificate", { CertificateArn: arn }),
						},
					]}
				>
					<Box>
						Delete certificate {cert.DomainName} permanently. A certificate that
						is in use by another resource cannot be deleted.
					</Box>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
