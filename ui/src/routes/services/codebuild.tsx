import { Alert, Link, SpaceBetween, Tabs } from "@cloudscape-design/components";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import { ConsoleLayout } from "#/components/Console";
import { BuildDetails, ProjectDetails } from "#/components/codebuild/Details";
import { ProjectDialog } from "#/components/codebuild/ProjectDialog";
import { StartDialog } from "#/components/codebuild/StartDialog";
import { Action, Buttons, Rows } from "#/components/containers/Controls";
import { builds, codebuildCall, deleteBuilds, projects } from "#/lib/codebuild";
import { codebuildHref, pending, type Scope } from "#/lib/codebuildHelpers";
import { unixTime } from "#/lib/format";
import { validRegion } from "#/lib/s3";
import { useScope } from "#/lib/scope";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";
export const Route = createFileRoute("/services/codebuild")({
	validateSearch: (s: Record<string, unknown>) => ({
		account: String(s.account ?? ""),
		region: String(s.region ?? ""),
		project: String(s.project ?? ""),
		build: String(s.build ?? ""),
		tab: s.tab === "builds" ? "builds" : "projects",
	}),
	component: CodeBuildPage,
});
function CodeBuildPage() {
	const { account, region, canWrite, capabilities, inventory } = useScope(),
		search = Route.useSearch();
	const scope = { account, region },
		home = codebuildHref(scope);
	const enabled =
			!!capabilities.data?.pages.codebuild &&
			canWrite &&
			/^\d{12}$/.test(account) &&
			validRegion(region),
		docker = capabilities.data?.codebuild_runtime === "docker";
	return (
		<ConsoleLayout
			contentType={search.project || search.build ? "default" : "table"}
			navigationHeader={{ text: "CodeBuild", href: home }}
			activeHref={home}
			navigationItems={[
				{ type: "link", text: "Projects", href: home },
				{
					type: "link",
					text: "Build history",
					href: codebuildHref(scope, "", "", "builds"),
				},
				{ type: "link", text: "All services", href: "/services" },
			]}
			breadcrumbs={[
				{ text: "CodeBuild", href: home },
				...(search.project
					? [
							{
								text: search.project,
								href: codebuildHref(scope, search.project),
							},
						]
					: []),
				...(search.build
					? [
							{
								text: search.build,
								href: codebuildHref(
									scope,
									search.project,
									search.build,
									"builds",
								),
							},
						]
					: []),
			]}
			content={
				<SpaceBetween size="l">
					{!!(capabilities.error || inventory.error) && (
						<Alert type="error">{capabilities.error || inventory.error}</Alert>
					)}
					{capabilities.data && !capabilities.data.pages.codebuild && (
						<Alert type="warning">
							CodeBuild is not enabled on this server.
						</Alert>
					)}
					{!canWrite && (
						<Alert>
							Connect a console session for this account to manage resources.
						</Alert>
					)}
					<Alert
						header={
							docker ? "Docker runtime configured" : "Records-only runtime"
						}
					>
						{docker
							? "Builds run in local containers using NO_SOURCE or S3 ZIP sources. Compute classes are metadata. Git sources are stored as metadata; CodePipeline sources execute through CodePipeline."
							: "Projects are stored as records. No build containers run; StartBuild fails explicitly and no build history is created. Set codebuild.builds to docker to execute builds."}
					</Alert>
					<Workspace
						key={`${account}/${region}/${enabled}/${search.project}/${search.build}`}
						scope={scope}
						enabled={enabled}
						docker={docker}
						project={search.project}
						build={search.build}
						tab={search.tab}
						logsEnabled={enabled && !!capabilities.data?.pages.logs}
						onChanged={inventory.reload}
					/>
				</SpaceBetween>
			}
		/>
	);
}
function Workspace({
	scope,
	enabled,
	docker,
	project,
	build,
	tab,
	logsEnabled,
	onChanged,
}: {
	scope: Scope;
	enabled: boolean;
	docker: boolean;
	project: string;
	build: string;
	tab: string;
	logsEnabled: boolean;
	onChanged: () => void;
}) {
	const navigate = useNavigate(),
		follow = useFollow();
	const [changing, setChanging] = useState(true),
		[dialog, setDialog] = useState("");
	const poll = usePoll(
		async () => {
			if (!enabled) return { projects: [], builds: [] };
			const [ps, bs] = await Promise.all([projects(scope), builds(scope)]);
			setChanging(bs.some(pending));
			return { projects: ps, builds: bs };
		},
		changing ? 2000 : 0,
	);
	const p = poll.data?.projects.find((p) => p.Name === project),
		b = poll.data?.builds.find((b) => b.Id === build);
	const bs = (poll.data?.builds ?? []).filter(
		(b) => !project || b.ProjectName === project,
	);
	const saved = () => {
		setDialog("");
		setChanging(true);
		poll.reload();
		onChanged();
	};
	const go = (p = "", b = "", t = tab) =>
		navigate({ href: codebuildHref(scope, p, b, t) });
	const finished = bs.filter((b) => !pending(b));
	const buildRows = (
		<Rows
			title="Builds"
			items={bs}
			loading={poll.loading}
			emptyText="Open a project and choose Start build when Docker execution is enabled."
			actions={
				<Buttons>
					<Action onClick={poll.reload}>Refresh</Action>
					<Action
						disabled={!enabled || !finished.length}
						onClick={() => setDialog("batch")}
					>
						Delete completed builds
					</Action>
				</Buttons>
			}
			columns={[
				{
					id: "id",
					header: "Build",
					cell: (b) => (
						<Link
							href={codebuildHref(scope, project, b.Id, "builds")}
							onFollow={follow}
						>
							{b.Id}
						</Link>
					),
				},
				{
					id: "project",
					header: "Project",
					cell: (b) => (
						<Link href={codebuildHref(scope, b.ProjectName)} onFollow={follow}>
							{b.ProjectName}
						</Link>
					),
				},
				{ id: "status", header: "Status", cell: (b) => b.BuildStatus },
				{ id: "phase", header: "Phase", cell: (b) => b.CurrentPhase },
				{ id: "start", header: "Started", cell: (b) => unixTime(b.StartTime) },
				{ id: "end", header: "Ended", cell: (b) => unixTime(b.EndTime) },
			]}
		/>
	);
	return (
		<SpaceBetween size="m">
			{!!poll.error && <Alert type="error">{poll.error}</Alert>}
			{build ? (
				b ? (
					<SpaceBetween size="m">
						<Buttons>
							<Action onClick={() => go(project, "", "builds")}>
								Build history
							</Action>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action
								disabled={!enabled || !pending(b)}
								onClick={() => setDialog("stop")}
							>
								Stop build
							</Action>
							<Action
								disabled={!enabled || pending(b)}
								onClick={() => setDialog("deleteBuild")}
							>
								Delete build
							</Action>
						</Buttons>
						<BuildDetails
							key={`${b.Id}/${b.BuildStatus}`}
							build={b}
							scope={scope}
							logsEnabled={logsEnabled}
						/>
					</SpaceBetween>
				) : (
					!poll.loading && (
						<Alert>Build not found in this account and Region.</Alert>
					)
				)
			) : project ? (
				p ? (
					<SpaceBetween size="m">
						<Buttons>
							<Action onClick={() => go("", "", "projects")}>Projects</Action>
							<Action onClick={poll.reload}>Refresh</Action>
							<Action disabled={!enabled} onClick={() => setDialog("edit")}>
								Edit project
							</Action>
							<Action
								disabled={
									!enabled ||
									!docker ||
									!["NO_SOURCE", "S3"].includes(p.Source.Type)
								}
								onClick={() => setDialog("start")}
							>
								Start build
							</Action>
							<Action disabled={!enabled} onClick={() => setDialog("delete")}>
								Delete project
							</Action>
						</Buttons>
						<Tabs
							activeTabId={tab}
							onChange={({ detail }) => go(project, "", detail.activeTabId)}
							tabs={[
								{
									id: "projects",
									label: "Project",
									content: <ProjectDetails project={p} />,
								},
								{ id: "builds", label: "Build history", content: buildRows },
							]}
						/>
					</SpaceBetween>
				) : (
					!poll.loading && (
						<Alert>Project not found in this account and Region.</Alert>
					)
				)
			) : (
				<Tabs
					activeTabId={tab}
					onChange={({ detail }) => go("", "", detail.activeTabId)}
					tabs={[
						{
							id: "projects",
							label: "Projects",
							content: (
								<Rows
									title="Projects"
									items={poll.data?.projects ?? []}
									loading={poll.loading}
									emptyText="Choose Create project to add one."
									actions={
										<Buttons>
											<Action onClick={poll.reload}>Refresh</Action>
											<Action
												disabled={!enabled}
												onClick={() => setDialog("create")}
											>
												Create project
											</Action>
										</Buttons>
									}
									columns={[
										{
											id: "name",
											header: "Name",
											cell: (p) => (
												<Link
													href={codebuildHref(scope, p.Name)}
													onFollow={follow}
												>
													{p.Name}
												</Link>
											),
										},
										{
											id: "source",
											header: "Source",
											cell: (p) => p.Source.Type,
										},
										{
											id: "image",
											header: "Image",
											cell: (p) => p.Environment.Image,
										},
										{
											id: "created",
											header: "Created",
											cell: (p) => unixTime(p.Created),
										},
										{
											id: "updated",
											header: "Updated",
											cell: (p) => unixTime(p.LastModified),
										},
									]}
								/>
							),
						},
						{ id: "builds", label: "Build history", content: buildRows },
					]}
				/>
			)}
			{(dialog === "create" || (dialog === "edit" && !!p)) && (
				<ProjectDialog
					scope={scope}
					project={dialog === "edit" ? p : undefined}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "start" && !!p && (
				<StartDialog
					scope={scope}
					name={p.Name}
					onClose={() => setDialog("")}
					onSaved={saved}
				/>
			)}
			{dialog === "delete" && !!p && (
				<ActionDialog
					title="Delete project"
					submit="Delete project"
					confirmation={p.Name}
					onClose={() => setDialog("")}
					onSaved={() => {
						saved();
						go("", "", "projects");
					}}
					plan={async () => [
						{
							label: "DeleteProject",
							run: () =>
								codebuildCall(scope, "DeleteProject", { Name: p.Name }),
						},
					]}
				>
					<Alert type="warning">
						Delete {p.Name}. Build history is retained.
					</Alert>
				</ActionDialog>
			)}
			{(dialog === "stop" || dialog === "deleteBuild") && !!b && (
				<ActionDialog
					title={dialog === "stop" ? "Stop build" : "Delete build"}
					submit={dialog === "stop" ? "Stop build" : "Delete build"}
					confirmation={dialog === "deleteBuild" ? b.Id : undefined}
					onClose={() => setDialog("")}
					onSaved={() => {
						saved();
						if (dialog === "deleteBuild") go(project, "", "builds");
					}}
					plan={async () => [
						{
							label: dialog === "stop" ? "StopBuild" : "BatchDeleteBuilds",
							run: () =>
								dialog === "stop"
									? codebuildCall(scope, "StopBuild", { Id: b.Id })
									: deleteBuilds(scope, [b.Id]),
						},
					]}
				>
					<Alert>{b.Id}</Alert>
				</ActionDialog>
			)}
			{dialog === "batch" && (
				<ActionDialog
					title="Delete completed builds"
					submit="Delete builds"
					confirmation={project || "delete builds"}
					onClose={() => setDialog("")}
					onSaved={saved}
					plan={async () => [
						{
							label: "BatchDeleteBuilds",
							run: () =>
								deleteBuilds(
									scope,
									finished.map((b) => b.Id),
								),
						},
					]}
				>
					<Alert type="warning">
						Delete these {finished.length} completed builds from the current
						account and Region{project ? ` for ${project}` : ""}. Logs and
						artifacts are retained.
						<pre>{finished.map((b) => b.Id).join("\n")}</pre>
					</Alert>
				</ActionDialog>
			)}
		</SpaceBetween>
	);
}
