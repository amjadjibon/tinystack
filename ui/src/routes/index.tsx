import {
	Alert,
	Box,
	Button,
	Checkbox,
	ContentLayout,
	Header,
	Icon,
	Link,
	Modal,
	SpaceBetween,
	StatusIndicator,
} from "@cloudscape-design/components";
import { createFileRoute } from "@tanstack/react-router";
import { type ReactNode, useEffect, useState } from "react";
import { ConsoleLayout } from "#/components/Console";
import { RequestStatus } from "#/components/RequestStatus";
import { ServiceIcon } from "#/components/ServiceIcon";
import { api } from "#/lib/api";
import { useHealth } from "#/lib/health";
import { useScope } from "#/lib/scope";
import { useServiceHistory } from "#/lib/serviceHistory";
import { consoleServiceNames, service } from "#/lib/services";
import { useFollow } from "#/lib/useFollow";
import { usePoll } from "#/lib/usePoll";

export const Route = createFileRoute("/")({ component: Home });
const widgetLabels = {
	recent: "Recently visited",
	welcome: "Welcome to TinyStack",
	health: "TinyStack health",
	resources: "Resources",
	requests: "Recent requests",
};
type WidgetID = keyof typeof widgetLabels;
const defaults = Object.keys(widgetLabels) as WidgetID[];
const layoutKey = "tinystack.console.home-widgets";

function Home() {
	const health = useHealth();
	const { inventory, account, region } = useScope();
	const requests = usePoll(api.requests);
	const recent = useServiceHistory().filter((name) =>
		consoleServiceNames(health.data?.services ?? []).includes(name),
	);
	const follow = useFollow();
	const [visible, setVisible] = useState<WidgetID[]>(defaults);
	const [draft, setDraft] = useState<WidgetID[] | null>(null);
	useEffect(() => {
		try {
			const saved = JSON.parse(localStorage.getItem(layoutKey) ?? "null");
			if (Array.isArray(saved))
				setVisible(defaults.filter((id) => saved.includes(id)));
		} catch {
			/* The default layout works without saved preferences. */
		}
	}, []);
	const saveLayout = (next: WidgetID[]) => {
		setVisible(next);
		try {
			localStorage.setItem(layoutKey, JSON.stringify(next));
		} catch {
			/* Keep the session's layout. */
		}
	};
	const resources = (inventory.data ?? []).filter(
		(r) =>
			r.account === account &&
			(!r.region || r.region === "global" || r.region === region),
	);
	const errors = requests.data?.filter((r) => r.status >= 400).length;
	const welcomeLinks = [
		{
			href: "/services",
			icon: "grid-view" as const,
			title: "Explore your services",
			text: "Find the AWS services available on your local endpoint.",
		},
		{
			href: "/resources",
			icon: "folder" as const,
			title: "Manage your resources",
			text: "Browse resources, inspect their configuration, and open service consoles.",
		},
		{
			href: "/requests",
			icon: "command-prompt" as const,
			title: "Inspect API requests",
			text: "See API calls, responses, and errors as you develop.",
		},
	];
	return (
		<ConsoleLayout
			contentType="dashboard"
			breadcrumbs={[]}
			navigationHeader={{ text: "Console home", href: "/" }}
			navigationItems={[
				{ type: "link", text: "All services", href: "/services" },
				{ type: "link", text: "All resources", href: "/resources" },
				{ type: "divider" },
				{ type: "link", text: "Request inspector", href: "/requests" },
				{ type: "link", text: "Deliveries", href: "/deliveries" },
				{ type: "link", text: "Metrics", href: "/metrics" },
			]}
			content={
				<ContentLayout
					header={
						<Header
							variant="h1"
							actions={
								<SpaceBetween direction="horizontal" size="s">
									<Button onClick={() => saveLayout(defaults)}>
										Reset to default layout
									</Button>
									<Button
										variant="primary"
										iconName="add-plus"
										onClick={() => setDraft([...visible])}
									>
										Add widgets
									</Button>
								</SpaceBetween>
							}
						>
							Console home
						</Header>
					}
				>
					<div className="console-dashboard">
						{visible.includes("recent") && (
							<Widget
								id="recent"
								wide
								action={
									<Button href="/services" onFollow={follow}>
										All services
									</Button>
								}
							>
								{recent.length ? (
									<ul className="dashboard-service-list">
										{recent.map((name) => (
											<li key={name}>
												<ServiceIcon name={name} size="large" />
												<Link
													href={`/services/${name}`}
													onFollow={follow}
													variant="primary"
												>
													{service(name).title}
												</Link>
											</li>
										))}
									</ul>
								) : (
									<div className="dashboard-empty">
										<Icon name="history" size="large" />
										<Box variant="p">
											Your recently visited services appear here.
										</Box>
										<Link href="/services" onFollow={follow}>
											Explore all services
										</Link>
									</div>
								)}
							</Widget>
						)}
						{visible.includes("welcome") && (
							<Widget id="welcome">
								<div className="dashboard-welcome">
									{welcomeLinks.map((item) => (
										<div key={item.href} className="dashboard-welcome-row">
											<span className="dashboard-welcome-icon">
												<Icon name={item.icon} size="large" />
											</span>
											<div>
												<Link
													href={item.href}
													onFollow={follow}
													variant="primary"
												>
													{item.title}
												</Link>
												<p>{item.text}</p>
											</div>
										</div>
									))}
								</div>
							</Widget>
						)}
						{visible.includes("health") && (
							<Widget
								id="health"
								footer={
									<Link href="/metrics" onFollow={follow} variant="primary">
										View metrics
									</Link>
								}
							>
								<div className="dashboard-health-row">
									<span>Server status</span>
									<StatusIndicator
										type={
											health.data
												? "success"
												: health.error
													? "error"
													: "loading"
										}
									>
										{health.data
											? "Running"
											: health.error
												? "Unreachable"
												: "Connecting"}
									</StatusIndicator>
								</div>
								<div className="dashboard-health-row">
									<span>Available services</span>
									<strong className="dashboard-number">
										{health.data?.services.length ?? "—"}
									</strong>
								</div>
								<div className="dashboard-health-row">
									<span>Request errors</span>
									<strong className="dashboard-number">{errors ?? "—"}</strong>
									<small>In the retained request log</small>
								</div>
								<div className="dashboard-health-row">
									<span>Storage</span>
									<strong>{health.data?.storage ?? "—"}</strong>
								</div>
							</Widget>
						)}
						{visible.includes("resources") && (
							<Widget
								id="resources"
								wide
								action={
									<Button href="/resources" onFollow={follow}>
										View resources
									</Button>
								}
							>
								<p className="dashboard-description">
									Resources in account {account || "…"} · {region || "…"},
									including global services.
								</p>
								{inventory.error ? (
									<Alert type="error">{inventory.error}</Alert>
								) : (
									<div className="dashboard-resource-counts">
										<div>
											<span>Total resources</span>
											<strong className="dashboard-number">
												{inventory.data ? resources.length : "—"}
											</strong>
										</div>
										<div>
											<span>Services with resources</span>
											<strong className="dashboard-number">
												{inventory.data
													? new Set(resources.map((r) => r.service)).size
													: "—"}
											</strong>
										</div>
									</div>
								)}
								<div className="dashboard-resource-links">
									<Link href="/services/iam" onFollow={follow}>
										Identity and Access Management
									</Link>
									<Link href="/services/s3" onFollow={follow}>
										Amazon S3
									</Link>
								</div>
							</Widget>
						)}
						{visible.includes("requests") && (
							<Widget
								id="requests"
								wide
								action={
									<Button href="/requests" onFollow={follow}>
										View all requests
									</Button>
								}
							>
								{requests.error ? (
									<Alert type="error">{requests.error}</Alert>
								) : !requests.data?.length ? (
									<Box color="text-body-secondary">
										API requests appear here when you use TinyStack.
									</Box>
								) : (
									<ul className="dashboard-request-list">
										{requests.data.slice(0, 5).map((r) => (
											<li key={r.request_id}>
												{r.service && (
													<ServiceIcon name={r.service} size="small" />
												)}
												<span>{r.operation || `${r.method} ${r.path}`}</span>
												<RequestStatus status={r.status} />
											</li>
										))}
									</ul>
								)}
							</Widget>
						)}
					</div>
					{!visible.length && (
						<Box textAlign="center" padding="xxl">
							Choose Add widgets to customize your console home.
						</Box>
					)}
					{draft && (
						<Modal
							visible
							header="Add widgets"
							onDismiss={() => setDraft(null)}
							footer={
								<Box float="right">
									<SpaceBetween direction="horizontal" size="xs">
										<Button variant="link" onClick={() => setDraft(null)}>
											Cancel
										</Button>
										<Button
											variant="primary"
											onClick={() => {
												saveLayout(draft);
												setDraft(null);
											}}
										>
											Save layout
										</Button>
									</SpaceBetween>
								</Box>
							}
						>
							<SpaceBetween size="m">
								<Box>Select the widgets to display on your console home.</Box>
								{defaults.map((id) => (
									<Checkbox
										key={id}
										checked={draft.includes(id)}
										onChange={({ detail }) =>
											setDraft(
												detail.checked
													? defaults.filter(
															(item) => item === id || draft.includes(item),
														)
													: draft.filter((item) => item !== id),
											)
										}
									>
										{widgetLabels[id]}
									</Checkbox>
								))}
							</SpaceBetween>
						</Modal>
					)}
				</ContentLayout>
			}
		/>
	);
}

function Widget({
	id,
	wide = false,
	action,
	footer,
	children,
}: {
	id: WidgetID;
	wide?: boolean;
	action?: ReactNode;
	footer?: ReactNode;
	children: ReactNode;
}) {
	return (
		<section
			className={`dashboard-widget ${wide ? "dashboard-widget-wide" : ""} dashboard-widget-${id}`}
			aria-labelledby={`widget-${id}`}
		>
			<div className="dashboard-widget-header">
				<h2 id={`widget-${id}`}>{widgetLabels[id]}</h2>
				{action}
			</div>
			<div className="dashboard-widget-content">{children}</div>
			{footer && <div className="dashboard-widget-footer">{footer}</div>}
		</section>
	);
}
