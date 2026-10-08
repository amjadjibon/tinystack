import {
	Badge,
	Box,
	ColumnLayout,
	Container,
	Header,
	Link,
	SpaceBetween,
} from "@cloudscape-design/components";
import { ServiceIcon } from "#/components/ServiceIcon";
import type { Resource } from "#/lib/api";
import { categories, service } from "#/lib/services";
import { useFollow } from "#/lib/useFollow";

export function ServiceCatalog({
	names,
	resources,
}: {
	names: string[];
	resources: Resource[];
}) {
	const follow = useFollow();
	const counts = new Map<string, number>();
	for (const r of resources)
		counts.set(r.service, (counts.get(r.service) ?? 0) + 1);

	const groups = new Map<string, string[]>();
	for (const name of names) {
		const { category } = service(name);
		groups.set(category, [...(groups.get(category) ?? []), name]);
	}

	const order = (c: string) =>
		categories.includes(c) ? categories.indexOf(c) : categories.length;
	const sorted = [...groups.entries()].sort(([a], [b]) => order(a) - order(b));

	return (
		<Container
			header={
				<Header
					variant="h2"
					counter={`(${names.length})`}
					description="Every service this TinyStack serves."
				>
					Services
				</Header>
			}
		>
			<ColumnLayout columns={3} variant="text-grid">
				{sorted.map(([category, members]) => (
					<SpaceBetween key={category} size="xs">
						<Box variant="h3">{category}</Box>
						{members
							.sort((a, b) => service(a).title.localeCompare(service(b).title))
							.map((name) => (
								<span
									key={name}
									style={{ display: "flex", alignItems: "center", gap: 8 }}
								>
									<ServiceIcon name={name} />
									<Link href={`/services/${name}`} onFollow={follow}>
										{service(name).title}
									</Link>
									{counts.get(name) ? (
										<Badge color="blue">{counts.get(name)}</Badge>
									) : null}
								</span>
							))}
					</SpaceBetween>
				))}
			</ColumnLayout>
		</Container>
	);
}
