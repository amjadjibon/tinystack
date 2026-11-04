import { Icon } from "@cloudscape-design/components";
import { service } from "#/lib/services";

const sizes = { small: 16, medium: 24, large: 32 };

export function ServiceIcon({
	name,
	size = "medium",
}: {
	name: string;
	size?: keyof typeof sizes;
}) {
	const { icon } = service(name);
	const px = sizes[size];
	if (!icon)
		return (
			<Icon name="settings" size={size === "small" ? "small" : "medium"} />
		);

	return (
		<img
			src={`/aws-icons/${icon}.svg`}
			alt=""
			width={px}
			height={px}
			style={{ borderRadius: 0, verticalAlign: "middle", flexShrink: 0 }}
			draggable={false}
		/>
	);
}

// ServiceLabel is a service's icon next to its console name.
export function ServiceLabel({ name }: { name: string }) {
	return (
		<span
			style={{
				display: "inline-flex",
				alignItems: "center",
				gap: 8,
				maxWidth: "100%",
			}}
		>
			<ServiceIcon name={name} size="small" />
			<span
				style={{
					minWidth: 0,
					overflow: "hidden",
					textOverflow: "ellipsis",
					whiteSpace: "nowrap",
				}}
				title={service(name).title}
			>
				{service(name).title}
			</span>
		</span>
	);
}
