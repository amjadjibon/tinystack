import {
	Alert,
	Box,
	Button,
	Container,
	Header,
	SpaceBetween,
} from "@cloudscape-design/components";
import { useState } from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { Details } from "#/components/elbv2/Shared";
import { Pick } from "#/components/memorydb/Fields";
import {
	type AppConfigScope,
	fetchConfiguration,
	listEnvironments,
	listProfiles,
} from "#/lib/appconfig";
import {
	type Application,
	bytesText,
	decodeContent,
	type FetchedConfiguration,
} from "#/lib/appconfigHelpers";
import { errorText } from "#/lib/consoleActions";
import { usePoll } from "#/lib/usePoll";

// FetchConfig reads the configuration deployed to an environment through the data plane,
// the way an application using AppConfig would.
export function FetchConfig({
	scope,
	application,
	enabled,
}: {
	scope: AppConfigScope;
	application: Application;
	enabled: boolean;
}) {
	const environments = usePoll(
		() =>
			enabled ? listEnvironments(scope, application.Id) : Promise.resolve([]),
		0,
	);
	const profiles = usePoll(
		() => (enabled ? listProfiles(scope, application.Id) : Promise.resolve([])),
		0,
	);
	const [environment, setEnvironment] = useState("");
	const [profile, setProfile] = useState("");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [result, setResult] = useState<FetchedConfiguration>();
	const fetchNow = async () => {
		setBusy(true);
		setError("");
		setResult(undefined);
		try {
			setResult(
				await fetchConfiguration(scope, {
					application: application.Id,
					environment,
					profile,
				}),
			);
		} catch (err) {
			setError(errorText(err));
		} finally {
			setBusy(false);
		}
	};
	const content = result?.Configuration
		? decodeContent(result.Configuration)
		: undefined;
	return (
		<Container
			header={
				<Header
					variant="h2"
					description="Starts a configuration session and polls it once, as an application using the AppConfig data plane would. The first poll returns what is deployed now."
				>
					Fetch configuration
				</Header>
			}
		>
			<SpaceBetween size="m">
				{(environments.error || profiles.error) && (
					<Alert type="error">{environments.error || profiles.error}</Alert>
				)}
				<Pick
					label="Environment"
					value={environment}
					options={(environments.data ?? []).map((e) => ({
						value: e.Id,
						label: e.Name ?? e.Id,
					}))}
					placeholder="Choose an environment"
					onChange={(id) => {
						setEnvironment(id);
						setResult(undefined);
					}}
				/>
				<Pick
					label="Configuration profile"
					value={profile}
					options={(profiles.data ?? []).map((p) => ({
						value: p.Id,
						label: p.Name ?? p.Id,
					}))}
					placeholder="Choose a configuration profile"
					onChange={(id) => {
						setProfile(id);
						setResult(undefined);
					}}
				/>
				<Box>
					<Button
						variant="primary"
						loading={busy}
						disabled={!enabled || !environment || !profile}
						onClick={fetchNow}
					>
						Fetch configuration
					</Button>
				</Box>
				{error && (
					<Alert type="error" header="Fetch failed">
						{error}
					</Alert>
				)}
				{result && !content && (
					<Alert type="info">
						Nothing is deployed to this environment for this configuration
						profile.
					</Alert>
				)}
				{result && content && (
					<SpaceBetween size="m">
						<Details
							items={[
								["Content type", result.ContentType],
								["Version label", result.VersionLabel],
								["Size", bytesText(content.size)],
								[
									"Poll again after",
									result.NextPollIntervalInSeconds === undefined
										? undefined
										: `${result.NextPollIntervalInSeconds} seconds`,
								],
							]}
						/>
						{content.text === undefined && (
							<Alert type="info">
								This content is not text, so it is shown as base64.
							</Alert>
						)}
						<CodeBlock text={content.text ?? result.Configuration ?? ""} />
					</SpaceBetween>
				)}
			</SpaceBetween>
		</Container>
	);
}
