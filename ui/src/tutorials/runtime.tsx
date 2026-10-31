import {
	Alert,
	AnnotationContext,
	Box,
	Button,
	Cards,
	Container,
	CopyToClipboard,
	Header,
	SpaceBetween,
	StatusIndicator,
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
import { ServiceIcon } from "#/components/ServiceIcon";
import { useHealth } from "#/lib/health";
import { useScope } from "#/lib/scope";
import { service } from "#/lib/services";
import { fetchSnapshot } from "#/lib/tutorials";
import { usePoll } from "#/lib/usePoll";
import { Cleanup } from "./Cleanup";
import { type Context, check } from "./checks";
import { tutorials } from "./index";
import { fresh, load, type Progress, save } from "./progress";
import { expand, type Step, type Tutorial } from "./types";

const drawerSizeKey = "tinystack.console.tutorial-drawer-size";
function useTutorialState() {
	const { account, region } = useScope();
	const health = useHealth();
	const [checkError, setCheckError] = useState<string>();
	const [open, setOpen] = useState(false);
	const [drawerSize, setSize] = useState(() => {
		try {
			return Number(localStorage.getItem(drawerSizeKey)) || 420;
		} catch {
			return 420;
		}
	});
	const setDrawerSize = (size: number) => {
		setSize(size);
		try {
			localStorage.setItem(drawerSizeKey, String(Math.round(size)));
		} catch {
			/* The size lasts for this session. */
		}
	};
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
		drawerSize,
		setDrawerSize,
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
				Run this command in CloudShell, or in a terminal with aws-local, which
				always targets TinyStack, so nothing reaches AWS. Outside CloudShell,
				use credentials for account {ctx.account}.
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
			{t.step && (
				<Container header={<Header variant="h3">{t.step.title}</Header>}>
					<SpaceBetween size="s">
						<StepContent step={t.step} ctx={t.ctx} />
						<LiveCheck />
						<SpaceBetween direction="horizontal" size="xs">
							<Button onClick={t.go}>Go there</Button>
							<Button onClick={t.previous} disabled={t.progress?.step === 0}>
								Previous
							</Button>
							<Button variant="primary" onClick={t.advance}>
								Next
							</Button>
						</SpaceBetween>
					</SpaceBetween>
				</Container>
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
function status(t: ReturnType<typeof useTutorials>, def: Tutorial) {
	const record = t.records[def.id];
	if (record?.finishedAt)
		return <StatusIndicator type="success">Completed</StatusIndicator>;
	if (!t.available(def))
		return <StatusIndicator type="stopped">Unavailable</StatusIndicator>;
	if (!record)
		return <StatusIndicator type="pending">Not started</StatusIndicator>;
	const total = def.tasks.flatMap((v) => v.steps).length;
	return (
		<StatusIndicator type="in-progress">
			{record.completed.length} of {total} steps
		</StatusIndicator>
	);
}
function StartButton({ def }: { def: Tutorial }) {
	const t = useTutorials();
	return (
		<Button disabled={!t.available(def)} onClick={() => t.start(def)}>
			{t.records[def.id] ? "Resume" : "Start"}
		</Button>
	);
}
export function TutorialList() {
	const t = useTutorials();
	return (
		<ul className="dashboard-tutorial-list">
			{tutorials.map((def) => (
				<li key={def.id}>
					<ServiceIcon name={def.services[0]} size="large" />
					<div>
						<strong>{def.title}</strong>
						<Box variant="small" color="text-body-secondary">
							{def.duration} · {status(t, def)}
						</Box>
					</div>
					<StartButton def={def} />
				</li>
			))}
		</ul>
	);
}
export function TutorialCards() {
	const t = useTutorials();
	return (
		<Cards
			items={tutorials}
			trackBy="id"
			cardsPerRow={[{ cards: 1 }, { minWidth: 700, cards: 2 }]}
			cardDefinition={{
				header: (def) => def.title,
				sections: [
					{ id: "description", content: (def) => def.description },
					{
						id: "services",
						header: "Services",
						content: (def) => (
							<SpaceBetween direction="horizontal" size="xs">
								{def.services.map((s) => (
									<ServiceIcon key={s} name={s} size="small" />
								))}
								<span>
									{def.services.map((s) => service(s).title).join(", ")}
								</span>
							</SpaceBetween>
						),
					},
					{
						id: "duration",
						header: "Duration",
						content: (def) => def.duration,
						width: 50,
					},
					{
						id: "status",
						header: "Status",
						content: (def) => status(t, def),
						width: 50,
					},
					{ id: "start", content: (def) => <StartButton def={def} /> },
				],
			}}
		/>
	);
}
