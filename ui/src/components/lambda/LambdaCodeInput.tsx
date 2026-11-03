import {
	Autosuggest,
	FileUpload,
	FormField,
	Input,
	RadioGroup,
	SpaceBetween,
} from "@cloudscape-design/components";
import type { Resource } from "#/lib/api";
import type { CodeInput, LambdaScope } from "#/lib/lambda";

export function LambdaCodeInput({
	value,
	onChange,
	scope,
	resources,
	image,
}: {
	value: CodeInput;
	onChange: (value: CodeInput) => void;
	scope: LambdaScope;
	resources: Resource[];
	image: boolean;
}) {
	const set = (change: Partial<CodeInput>) => onChange({ ...value, ...change });
	return (
		<SpaceBetween size="l">
			{image ? (
				<FormField
					label="Container image URI"
					description="Docker must be able to pull this image on the TinyStack host."
				>
					<Input
						ariaLabel="Container image URI"
						value={value.image}
						onChange={({ detail }) => set({ image: detail.value })}
					/>
				</FormField>
			) : (
				<SpaceBetween size="l">
					<FormField label="Code source">
						<RadioGroup
							value={value.source}
							onChange={({ detail }) =>
								set({ source: detail.value as "zip" | "s3" })
							}
							items={[
								{ value: "zip", label: "Upload a ZIP file" },
								{ value: "s3", label: "Amazon S3 location" },
							]}
						/>
					</FormField>
					{value.source === "zip" ? (
						<FormField
							label="Deployment package"
							constraintText="ZIP file, up to 1 MiB. Use S3 for larger packages."
						>
							<FileUpload
								value={value.files}
								onChange={({ detail }) => set({ files: detail.value })}
								accept=".zip"
								showFileSize
								showFileLastModified
								i18nStrings={{
									uploadButtonText: () => "Choose file",
									dropzoneText: () => "Drop a ZIP file",
									removeFileAriaLabel: (i) => `Remove file ${i + 1}`,
									limitShowFewer: "Show fewer",
									limitShowMore: "Show more",
									errorIconAriaLabel: "Error",
								}}
							/>
						</FormField>
					) : (
						<SpaceBetween size="l">
							<FormField
								label="S3 bucket"
								description="The package is copied from this bucket in the selected account and Region."
							>
								<Autosuggest
									ariaLabel="Code S3 bucket"
									value={value.bucket}
									onChange={({ detail }) => set({ bucket: detail.value })}
									options={resources
										.filter(
											(r) =>
												r.service === "s3" &&
												r.type === "bucket" &&
												r.account === scope.account &&
												r.region === scope.region,
										)
										.map((r) => ({ value: r.name }))}
								/>
							</FormField>
							<FormField label="S3 object key">
								<Input
									ariaLabel="Code S3 object key"
									value={value.key}
									onChange={({ detail }) => set({ key: detail.value })}
								/>
							</FormField>
							<FormField label="Object version — optional">
								<Input
									ariaLabel="Code S3 object version"
									value={value.version}
									onChange={({ detail }) => set({ version: detail.value })}
								/>
							</FormField>
						</SpaceBetween>
					)}
				</SpaceBetween>
			)}
		</SpaceBetween>
	);
}
