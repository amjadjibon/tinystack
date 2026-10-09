import {
	Alert,
	Box,
	Button,
	Container,
	ContentLayout,
	Form,
	FormField,
	Header,
	Input,
	RadioGroup,
	Select,
	SpaceBetween,
	Textarea,
} from "@cloudscape-design/components";
import { useState } from "react";
import type { Resource } from "#/lib/api";
import { errorText, stringMap } from "#/lib/consoleActions";
import {
	codeDefaults,
	configurationInput,
	deploymentCode,
	type LambdaScope,
	lambdaCall,
	settingsDefaults,
} from "#/lib/lambda";
import { LambdaCodeInput } from "./LambdaCodeInput";
import { LambdaSettings } from "./LambdaSettings";

export function LambdaFunctionForm({
	scope,
	resources,
	onCancel,
	onSaved,
}: {
	scope: LambdaScope;
	resources: Resource[];
	onCancel: () => void;
	onSaved: (name: string) => void;
}) {
	const [name, setName] = useState("");
	const [image, setImage] = useState(false);
	const [architecture, setArchitecture] = useState("x86_64");
	const [settings, setSettings] = useState(settingsDefaults);
	const [code, setCode] = useState(codeDefaults);
	const [tags, setTags] = useState("{}");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const save = async () => {
		if (busy) return;
		setBusy(true);
		setError("");
		try {
			if (!/^[A-Za-z0-9_-]{1,64}$/.test(name))
				throw new Error(
					"Use 1–64 letters, numbers, hyphens or underscores for the function name.",
				);
			await lambdaCall(scope, "CreateFunction", {
				FunctionName: name,
				PackageType: image ? "Image" : "Zip",
				...configurationInput(settings, image),
				Code: await deploymentCode(code),
				Architectures: [architecture],
				Tags: stringMap(tags, "Tags"),
			});
			onSaved(name);
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	return (
		<ContentLayout header={<Header variant="h1">Create function</Header>}>
			<Form
				errorText={error}
				actions={
					<SpaceBetween direction="horizontal" size="xs">
						<Button disabled={busy} onClick={onCancel}>
							Cancel
						</Button>
						<Button variant="primary" loading={busy} onClick={save}>
							Create function
						</Button>
					</SpaceBetween>
				}
			>
				<fieldset disabled={busy} style={{ border: 0, margin: 0, padding: 0 }}>
					<SpaceBetween size="l">
						<Container header={<Header variant="h2">Basic information</Header>}>
							<SpaceBetween size="l">
								<FormField label="Function name">
									<Input
										ariaLabel="Function name"
										value={name}
										onChange={({ detail }) => setName(detail.value)}
									/>
								</FormField>
								<FormField
									label="Package type"
									description="The package type cannot be changed after creation."
								>
									<RadioGroup
										value={image ? "image" : "zip"}
										onChange={({ detail }) => {
											setImage(detail.value === "image");
											setCode({
												...codeDefaults(),
												source: detail.value === "image" ? "image" : "zip",
											});
										}}
										items={[
											{ value: "zip", label: "ZIP archive" },
											{ value: "image", label: "Container image" },
										]}
									/>
								</FormField>
								<FormField label="Architecture">
									<Select
										selectedOption={{
											value: architecture,
											label: architecture,
										}}
										options={[
											{ value: "x86_64", label: "x86_64" },
											{ value: "arm64", label: "arm64" },
										]}
										onChange={({ detail }) =>
											setArchitecture(detail.selectedOption.value ?? "x86_64")
										}
									/>
								</FormField>
								<Box>
									AWS account: {scope.account} · AWS Region: {scope.region}
								</Box>
							</SpaceBetween>
						</Container>
						<Container
							header={<Header variant="h2">Runtime and configuration</Header>}
						>
							<LambdaSettings
								value={settings}
								onChange={setSettings}
								scope={scope}
								resources={resources}
								image={image}
							/>
						</Container>
						<Container header={<Header variant="h2">Function code</Header>}>
							<SpaceBetween size="l">
								<Alert type="info">
									Deploy a package containing your handler. Executing functions
									requires Docker on the TinyStack host.
								</Alert>
								<LambdaCodeInput
									value={code}
									onChange={setCode}
									scope={scope}
									resources={resources}
									image={image}
								/>
							</SpaceBetween>
						</Container>
						<Container header={<Header variant="h2">Tags — optional</Header>}>
							<FormField
								label="Tags (JSON)"
								description='Object of string values, for example {"team":"platform"}.'
							>
								<Textarea
									ariaLabel="Function tags"
									value={tags}
									rows={4}
									onChange={({ detail }) => setTags(detail.value)}
								/>
							</FormField>
						</Container>
					</SpaceBetween>
				</fieldset>
			</Form>
		</ContentLayout>
	);
}
