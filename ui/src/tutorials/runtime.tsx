import {
	Alert,
	AnnotationContext,
	Box,
	Button,
	Container,
	CopyToClipboard,
	Header,
	SpaceBetween,
	TutorialPanel,
	type TutorialPanelProps,
} from "@cloudscape-design/components";
import { useNavigate } from "@tanstack/react-router";
import {
	createContext,
	type ReactNode,
	useContext,
	useEffect,
	useMemo,
	useState,
} from "react";
import { CodeBlock } from "#/components/CodeBlock";
import { useHealth } from "#/lib/health";
import { useScope } from "#/lib/scope";
import { fetchSnapshot } from "#/lib/tutorials";
import { usePoll } from "#/lib/usePoll";
import { Cleanup } from "./Cleanup";
import { type Context, check } from "./checks";
import { tutorials } from "./index";
import { fresh, load, type Progress, save } from "./progress";
import { expand, type Step, type Tutorial } from "./types";

function useTutorialState() {
	const { account, region } = useScope();
	const health = useHealth();
	const [checkError, setCheckError] = useState<string>();
	const [open, setOpen] = useState(false);
	const [active, setActive] = useState<string | null>(null);
	const [revision, setRevision] = useState(0);
	const [records, setRecords] = useState<Record<string, Progress>>({});
	useEffect(() => {
		const refresh = () => {
			const next: Record<string, Progress> = {};
			for (const t of tutorials) {
				const p = load(localStorage, t.id, account, region);
				if (p) next[t.id] = p;
			}
			setRecords(next);
		};
		refresh();
		window.addEventListener("tutorial-reset", refresh);
		return () => window.removeEventListener("tutorial-reset", refresh);
	}, [account, region]);
	const tutorial = tutorials.find((t) => t.id === active);
	const progress = active ? records[active] : undefined;
	const ctx: Context = { account, region, names: progress?.names ?? {} };
	const steps = tutorial?.tasks.flatMap((t) => t.steps) ?? [];
	const step =
		progress && !progress.finishedAt ? steps[progress.step] : undefined;
	const update = (t: Tutorial, p: Progress) => {
		try {
			save(localStorage, t.id, account, region, p);
		} catch {
			/* Session progress works without storage. */
		}
		setRecords((r) => ({ ...r, [t.id]: p }));
		setRevision((v) => v + 1);
	};
	const available = (t: Tutorial) =>
		!!account &&
		!!region &&
		t.services.every((s) => health.data?.services.includes(s));
	const start = (t: Tutorial, restart = false) => {
		if (!available(t)) return;
		if (restart || !records[t.id]) update(t, fresh(t.id));
		setActive(t.id);
		setOpen(true);
	};
	const advance = () => {
		if (!tutorial || !progress || !step) return;
		const next = progress.step + 1;
		update(tutorial, {
			...progress,
			step: next,
			completed: [...new Set([...progress.completed, progress.step])],
			...(next >= steps.length ? { finishedAt: new Date().toISOString() } : {}),
		});
	};
	const previous = () => {
		if (tutorial && progress && progress.step > 0)
			update(tutorial, {
				...progress,
				step: progress.step - 1,
				finishedAt: undefined,
			});
	};
	const navigate = useNavigate();
	const go = () => {
		if (!step) return;
		const url = new URL(expand(step.route, ctx), window.location.origin);
		url.searchParams.set("account", account);
		url.searchParams.set("region", region);
		navigate({ href: url.pathname + url.search });
	};
	return {
		checkError,
		setCheckError,
		open,
		setOpen,
		active,
		setActive,
		tutorial,
		progress,
		ctx,
		step,
		steps,
		records,
		available,
		start,
		advance,
		previous,
		go,
		revision,
	};
}
const TutorialContext = createContext<ReturnType<
	typeof useTutorialState
> | null>(null);
export function useTutorials() {
	const value = useContext(TutorialContext);
	if (!value) throw new Error("TutorialProvider required");
	return value;
}
export function TutorialProvider({ children }: { children: ReactNode }) {
	const state = useTutorialState();
	return (
		<TutorialContext.Provider value={state}>
			{state.step && (
				<LiveObserver
					key={`${state.ctx.account}/${state.ctx.region}/${state.active}/${state.progress?.step}`}
				/>
			)}
			{children}
		</TutorialContext.Provider>
	);
}
function StepContent({ step, ctx }: { step: Step; ctx: Context }) {
	const createRegion =
		step.check === "bucket" && ctx.region !== "us-east-1"
			? ` --create-bucket-configuration LocationConstraint=${ctx.region}`
			: "";
	const command = `${expand(step.command, ctx)}${createRegion} --region ${ctx.region}`;
	return (
		<SpaceBetween size="s">
			<Box>{expand(step.content, ctx)}</Box>
			<Box>
				Run this single command in CloudShell, which supplies the local endpoint
				and selected account. For an external AWS CLI, set AWS_ENDPOINT_URL to
				TinyStack and use credentials for account {ctx.account}.
			</Box>
			<CodeBlock text={command} />
			<CopyToClipboard
				copyButtonText="Copy CLI commands"
				copySuccessText="Copied"
				copyErrorText="Copy failed"
				textToCopy={command}
			/>
		</SpaceBetween>
	);
}
export function TutorialAnnotations({ children }: { children: ReactNode }) {
	const t = useTutorials();
	// Cloudscape resets its internal position when currentTutorial changes. Present
	// the current step, keeping our persisted absolute position authoritative.
	// biome-ignore lint/correctness/useExhaustiveDependencies: only persisted progress and scope determine this object.
	const current = useMemo<TutorialPanelProps.Tutorial | null>(
		() =>
			t.tutorial && t.progress
				? {
						title: t.tutorial.title,
						description: t.tutorial.description,
						completed: !!t.progress.finishedAt,
						tasks: [
							{
								title: t.tutorial.tasks[0].title,
								steps: t.steps
									.slice(t.progress.step, t.progress.step + 1)
									.map((s) => ({
										...s,
										content: <StepContent step={s} ctx={t.ctx} />,
									})),
							},
						],
						completedScreenDescription: (
							<>
								You completed {t.tutorial.title}.{" "}
								<Button href={t.tutorial.learnMore} target="_blank">
									Learn more
								</Button>
								<Cleanup tutorial={t.tutorial} ctx={t.ctx} />
							</>
						),
						learnMoreUrl: t.tutorial.learnMore,
						// Names and scope determine expanded content; revision changes only on progress.
					}
				: null,
		[t.tutorial, t.progress, t.ctx.account, t.ctx.region, t.revision],
	);
	return (
		<AnnotationContext
			currentTutorial={current}
			onStartTutorial={({ detail }) => {
				const def = tutorials.find((d) => d.title === detail.tutorial.title);
				if (def) t.start(def, !!t.records[def.id]?.finishedAt);
			}}
			onExitTutorial={() => t.setActive(null)}
			onFinish={t.advance}
			onStepChange={({ detail }) => {
				if (detail.reason === "next") t.advance();
			}}
			i18nStrings={{
				nextButtonText: "Next",
				previousButtonText: "Previous",
				finishButtonText: "Next",
				labelDismissAnnotation: "Dismiss annotation",
				labelHotspot: () =>
					`Step ${(t.progress?.step ?? 0) + 1} of ${t.steps.length}`,
				stepCounterText: () =>
					`Step ${(t.progress?.step ?? 0) + 1} of ${t.steps.length}`,
				taskTitle: (_, title) => title,
			}}
		>
			{children}
		</AnnotationContext>
	);
}
export function TutorialDrawer() {
	const t = useTutorials();
	const list = tutorials.map((def) => ({
		...def,
		completed: !!t.records[def.id]?.finishedAt,
		prerequisitesNeeded: !t.available(def),
		prerequisitesAlert: "Required services or account/Region unavailable.",
		learnMoreUrl: def.learnMore,
		completedScreenDescription: "Tutorial complete.",
	}));
	return (
		<SpaceBetween size="m">
			<TutorialPanel
				tutorials={list}
				i18nStrings={{
					loadingText: "Loading tutorials",
					tutorialListTitle: "Tutorials",
					tutorialListDescription: "Learn with live console resources.",
					tutorialListDownloadLinkText: "Download",
					tutorialCompletedText: "Completed",
					learnMoreLinkText: "Learn more",
					startTutorialButtonText: "Start tutorial",
					restartTutorialButtonText: "Restart tutorial",
					completionScreenTitle: "Tutorial complete",
					feedbackLinkText: "Feedback",
					dismissTutorialButtonText: "Close tutorial",
					taskTitle: (i, title) => `${i + 1}. ${title}`,
					stepTitle: (i, title) => `${i + 1}. ${title}`,
					labelExitTutorial: "Exit tutorial",
					labelTotalSteps: (n) => `${n} steps`,
					labelLearnMoreExternalIcon: "Opens documentation",
					labelsTaskStatus: {
						pending: "Pending",
						"in-progress": "In progress",
						success: "Complete",
					},
				}}
			/>
			{t.tutorial && t.progress && (
				<>
					<Box>{t.tutorial.prerequisites}</Box>
					<Box>
						{t.progress.completed.length} of {t.steps.length} steps completed
					</Box>
					<ol>
						{t.steps.map((s, i) => (
							<li key={s.hotspotId}>
								{s.title}:{" "}
								{t.progress?.completed.includes(i)
									? "Completed"
									: i === t.progress?.step
										? "Current"
										: "Pending"}
							</li>
						))}
					</ol>
					{t.step && (
						<>
							<Header variant="h3">{t.step.title}</Header>
							<StepContent step={t.step} ctx={t.ctx} />
							<LiveCheck />
							<SpaceBetween direction="horizontal" size="xs">
								<Button onClick={t.go}>Go there</Button>
								<Button onClick={t.previous} disabled={t.progress.step === 0}>
									Previous
								</Button>
								<Button onClick={t.advance}>Next</Button>
							</SpaceBetween>
						</>
					)}
					<Button
						onClick={() => {
							if (t.tutorial) t.start(t.tutorial, true);
						}}
					>
						Restart
					</Button>
				</>
			)}
		</SpaceBetween>
	);
}
function LiveObserver() {
	const t = useTutorials();
	const kind = t.step?.check;
	const poll = usePoll(() => fetchSnapshot(kind), 2000);
	// biome-ignore lint/correctness/useExhaustiveDependencies: one result is checked once; callbacks change each render.
	useEffect(() => {
		t.setCheckError(poll.error);
		if (poll.data && kind && check(kind, poll.data, t.ctx)) t.advance();
	}, [poll.data, poll.error, kind]);
	return null;
}
function LiveCheck() {
	const { checkError } = useTutorials();
	return checkError ? (
		<Alert type="warning">
			{checkError}.{" "}
			{checkError.includes("403")
				? "Administrator access required for live checks. Use Next after completing the action."
				: "Live checks unavailable. Retry automatically, or use Next after completing the action."}
		</Alert>
	) : (
		<Box>
			Live checks run every 2 seconds. You can also complete this step manually
			with Next.
		</Box>
	);
}
export function TutorialCards({ home = false }: { home?: boolean }) {
	const t = useTutorials();
	return (
		<SpaceBetween size="m">
			{home && <Header variant="h2">Get started</Header>}
			{tutorials.slice(0, home ? 3 : undefined).map((def) => (
				<Container
					key={def.id}
					header={<Header variant="h2">{def.title}</Header>}
				>
					<SpaceBetween size="s">
						<Box>
							{def.description} {def.duration}
						</Box>
						<Box>{def.prerequisites}</Box>
						<Box>
							{t.records[def.id]?.finishedAt
								? "Completed"
								: `${t.records[def.id]?.completed.length ?? 0} of ${def.tasks.flatMap((v) => v.steps).length} steps completed`}
						</Box>
						{!t.available(def) && (
							<Box>Required services or account/Region unavailable.</Box>
						)}
						<Button disabled={!t.available(def)} onClick={() => t.start(def)}>
							{t.records[def.id] ? "Resume" : "Start"}
						</Button>
					</SpaceBetween>
				</Container>
			))}
		</SpaceBetween>
	);
}
