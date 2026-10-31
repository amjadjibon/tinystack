import { Box, Header, Table } from "@cloudscape-design/components";
import {
	noEchoValue,
	type StackOutput,
	type StackParameter,
} from "#/lib/cloudformationHelpers";

const empty = (text: string) => (
	<Box textAlign="center" padding="l">
		{text}
	</Box>
);

export function StackOutputs({ outputs }: { outputs: StackOutput[] }) {
	return (
		<Table
			items={outputs}
			trackBy="OutputKey"
			variant="embedded"
			header={<Header counter={`(${outputs.length})`}>Outputs</Header>}
			empty={empty("This stack has no outputs.")}
			columnDefinitions={[
				{
					id: "key",
					header: "Key",
					isRowHeader: true,
					cell: (o) => o.OutputKey,
				},
				{ id: "value", header: "Value", cell: (o) => o.OutputValue ?? "—" },
				{
					id: "description",
					header: "Description",
					cell: (o) => o.Description ?? "—",
				},
				{
					id: "export",
					header: "Export name",
					cell: (o) => o.ExportName ?? "—",
				},
			]}
		/>
	);
}

// StackParameters never shows a NoEcho value: the service answers ****, which is labelled as hidden.
export function StackParameters({
	parameters,
}: {
	parameters: StackParameter[];
}) {
	return (
		<Table
			items={parameters}
			trackBy="ParameterKey"
			variant="embedded"
			header={<Header counter={`(${parameters.length})`}>Parameters</Header>}
			empty={empty("This stack has no parameters.")}
			columnDefinitions={[
				{
					id: "key",
					header: "Key",
					isRowHeader: true,
					cell: (p) => p.ParameterKey,
				},
				{
					id: "value",
					header: "Value",
					cell: (p) =>
						p.ParameterValue === noEchoValue
							? "•••••• (hidden, NoEcho)"
							: (p.ParameterValue ?? "—"),
				},
				{
					id: "resolved",
					header: "Resolved value",
					cell: (p) => p.ResolvedValue ?? "—",
				},
			]}
		/>
	);
}
