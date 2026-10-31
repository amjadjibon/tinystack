import { ContentLayout, Header } from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { ConsoleLayout } from "#/components/Console";
import { TutorialCards } from "#/tutorials/runtime";
export const Route = createFileRoute("/tutorials")({ component: Tutorials });
function Tutorials() {
	return (
		<ConsoleLayout
			contentType="default"
			breadcrumbs={[{ text: "Tutorials", href: "/tutorials" }]}
			content={
				<ContentLayout header={<Header variant="h1">Tutorials</Header>}>
					<TutorialCards />
				</ContentLayout>
			}
		/>
	);
}
