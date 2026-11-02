import {
	Box,
	Button,
	Checkbox,
	FormField,
	Input,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useRef, useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { type ACMScope, acmCall } from "#/lib/acm";
import {
	addName,
	removeName,
	requestInput,
	requestKeyAlgorithms,
	validationMethods,
} from "#/lib/acmHelpers";
import { Choice } from "./Fields";

export function RequestDialog({
	scope,
	onClose,
	onSaved,
}: {
	scope: ACMScope;
	onClose: () => void;
	onSaved: (arn: string) => void;
}) {
	const [domain, setDomain] = useState(""),
		[names, setNames] = useState<string[]>([]),
		[next, setNext] = useState(""),
		[nameError, setNameError] = useState(""),
		[method, setMethod] = useState("DNS"),
		[algorithm, setAlgorithm] = useState("RSA_2048"),
		[exportable, setExportable] = useState(false),
		[transparency, setTransparency] = useState(true),
		[tags, setTags] = useState("");
	const created = useRef("");
	const add = () => {
		const result = addName(names, domain, next);
		setNameError(result.error);
		if (!result.error) {
			setNames(result.names);
			setNext("");
		}
	};
	return (
		<ActionDialog
			title="Request certificate"
			submit="Request certificate"
			onClose={onClose}
			onSaved={() => onSaved(created.current)}
			plan={async () => {
				const input = requestInput({
					domain,
					names,
					method,
					algorithm,
					exportable,
					transparency,
					tags,
				});
				return [
					{
						label: "Request certificate",
						run: async () => {
							const out = await acmCall<{ CertificateArn: string }>(
								scope,
								"RequestCertificate",
								input,
							);
							created.current = out.CertificateArn;
						},
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<Box color="text-body-secondary">
					TinyStack issues the certificate immediately from its local
					certificate authority. Validation records are shown for reference and
					are not checked.
				</Box>
				<FormField label="Domain name">
					<Input
						ariaLabel="Domain name"
						placeholder="example.com or *.example.com"
						value={domain}
						onChange={({ detail }) => setDomain(detail.value)}
					/>
				</FormField>
				<FormField
					label="Additional names"
					description="Subject alternative names. Choose Add name after typing each one."
					errorText={nameError}
				>
					<SpaceBetween size="xs">
						<SpaceBetween direction="horizontal" size="xs">
							<Input
								ariaLabel="Additional name"
								placeholder="www.example.com"
								value={next}
								onChange={({ detail }) => setNext(detail.value)}
								onKeyDown={({ detail }) => {
									if (detail.key === "Enter") add();
								}}
							/>
							<Button onClick={add}>Add name</Button>
						</SpaceBetween>
						{names.map((name) => (
							<SpaceBetween key={name} direction="horizontal" size="xs">
								<Box>{name}</Box>
								<Button
									variant="inline-link"
									ariaLabel={`Remove ${name}`}
									onClick={() => setNames(removeName(names, name))}
								>
									Remove
								</Button>
							</SpaceBetween>
						))}
					</SpaceBetween>
				</FormField>
				<Choice
					label="Validation method"
					value={method}
					options={validationMethods}
					onChange={setMethod}
				/>
				<Choice
					label="Key algorithm"
					value={algorithm}
					options={requestKeyAlgorithms}
					onChange={setAlgorithm}
				/>
				<Checkbox
					checked={exportable}
					onChange={({ detail }) => setExportable(detail.checked)}
					description="Keeps the private key so the certificate can be exported. This cannot be changed later."
				>
					Allow export
				</Checkbox>
				<Checkbox
					checked={transparency}
					onChange={({ detail }) => setTransparency(detail.checked)}
				>
					Certificate transparency logging
				</Checkbox>
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
			</SpaceBetween>
		</ActionDialog>
	);
}
