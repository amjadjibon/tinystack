import { SpaceBetween } from "@cloudscape-design/components";
import { useState } from "react";
import { ActionDialog } from "#/components/ActionDialog";
import type { Resource } from "#/lib/api";
import { jsonObject } from "#/lib/consoleActions";
import {
	categoryName,
	configuration,
	costCall,
	type Definition,
	jsonArray,
	required,
	subscriberAddresses,
} from "#/lib/cost";
import { expression, monitorSpecification } from "#/lib/costValidation";
import { CostInput, CostJSON, CostSelect } from "./CostFields";
export function ExplorerForm({
	account,
	section,
	resource,
	onClose,
	onSaved,
}: {
	account: string;
	section: string;
	resource?: Resource;
	onClose: () => void;
	onSaved: () => void;
}) {
	const config = resource ? configuration(resource) : {};
	const category = section === "costcategory",
		monitor = section === "anomalymonitor";
	const [name, setName] = useState(resource?.name ?? "");
	const [type, setType] = useState(String(config.MonitorType ?? "DIMENSIONAL"));
	const [dimension, setDimension] = useState(
		String(config.MonitorDimension ?? "SERVICE"),
	);
	const [spec, setSpec] = useState(
		JSON.stringify(
			config.MonitorSpecification ?? {
				Dimensions: { Key: "LINKED_ACCOUNT", Values: [account] },
			},
			null,
			2,
		),
	);
	const [frequency, setFrequency] = useState(
		String(config.Frequency ?? "DAILY"),
	);
	const [arns, setArns] = useState(
		((config.MonitorArnList ?? []) as string[]).join("\n"),
	);
	const [addresses, setAddresses] = useState(
		((config.Subscribers ?? []) as { Address: string }[])
			.map((s) => s.Address)
			.join("\n"),
	);
	const [threshold, setThreshold] = useState(
		JSON.stringify(
			config.ThresholdExpression ?? {
				Dimensions: {
					Key: "ANOMALY_TOTAL_IMPACT_ABSOLUTE",
					MatchOptions: ["GREATER_THAN_OR_EQUAL"],
					Values: ["100"],
				},
			},
			null,
			2,
		),
	);
	const [rules, setRules] = useState(
		JSON.stringify(
			config.Rules ?? [
				{
					Type: "REGULAR",
					Value: "Compute",
					Rule: { Dimensions: { Key: "SERVICE_CODE", Values: ["AmazonEC2"] } },
				},
			],
			null,
			2,
		),
	);
	const [fallback, setFallback] = useState(String(config.DefaultValue ?? ""));
	const noun = category
		? "cost category"
		: monitor
			? "anomaly monitor"
			: "anomaly subscription";
	return (
		<ActionDialog
			title={`${resource ? "Edit" : "Create"} ${noun}`}
			submit={resource ? "Save changes" : `Create ${noun}`}
			onClose={onClose}
			onSaved={onSaved}
			plan={async () => {
				let operation: string, input: Definition;
				if (category) {
					if (config.DefaultValue && !fallback)
						throw new Error(
							"A stored default value cannot be removed; set another value.",
						);
					const Rules = jsonArray(rules, "Rules", 1, 500);
					for (const rule of Rules) {
						if (rule.Type !== "INHERITED_VALUE") {
							categoryName(String(rule.Value ?? ""));
							if (rule.Rule) expression(rule.Rule as Definition, "category");
							if (!rule.Rule)
								throw new Error("Regular rules require a Rule expression.");
						}
					}
					input = {
						RuleVersion: "CostCategoryExpression.v1",
						Rules,
						...(fallback ? { DefaultValue: categoryName(fallback) } : {}),
					};
					if (resource) {
						input.CostCategoryArn = resource.arn;
						if (config.SplitChargeRules)
							input.SplitChargeRules = config.SplitChargeRules;
					} else input.Name = categoryName(name);
					operation = resource
						? "UpdateCostCategoryDefinition"
						: "CreateCostCategoryDefinition";
				} else if (monitor) {
					const MonitorName = required(name, "Monitor name");
					operation = resource
						? "UpdateAnomalyMonitor"
						: "CreateAnomalyMonitor";
					if (resource) input = { MonitorArn: resource.arn, MonitorName };
					else {
						const AnomalyMonitor: Definition = {
							MonitorName,
							MonitorType: type,
						};
						if (type === "DIMENSIONAL")
							AnomalyMonitor.MonitorDimension = dimension;
						if (
							type === "CUSTOM" ||
							["TAG", "COST_CATEGORY"].includes(dimension)
						) {
							AnomalyMonitor.MonitorSpecification = monitorSpecification(
								jsonObject(spec, "Monitor specification"),
								type,
								dimension,
							);
						}
						input = { AnomalyMonitor };
					}
				} else {
					const MonitorArnList = arns
						.split("\n")
						.map((v) => v.trim())
						.filter(Boolean);
					if (
						!MonitorArnList.length ||
						MonitorArnList.some(
							(v) =>
								!new RegExp(
									`^arn:aws:ce::${account}:anomalymonitor/[a-zA-Z0-9-]+$`,
								).test(v),
						)
					)
						throw new Error(
							"Provide monitor ARNs in this account, one per line.",
						);
					const subscriberType = frequency === "IMMEDIATE" ? "SNS" : "EMAIL";
					const sub = {
						SubscriptionName: required(name, "Subscription name"),
						Frequency: frequency,
						MonitorArnList,
						ThresholdExpression: expression(
							jsonObject(threshold, "Threshold expression"),
							"threshold",
						),
						Subscribers: subscriberAddresses(addresses, subscriberType).map(
							(Address) => ({ Type: subscriberType, Address }),
						),
					};
					operation = resource
						? "UpdateAnomalySubscription"
						: "CreateAnomalySubscription";
					input = resource
						? { SubscriptionArn: resource.arn, ...sub }
						: { AnomalySubscription: sub };
				}
				return [
					{
						label: `${resource ? "Update" : "Create"} ${noun}`,
						run: () => costCall("costexplorer", account, operation, input),
					},
				];
			}}
		>
			<SpaceBetween size="m">
				<CostInput
					label="Name"
					value={name}
					onChange={setName}
					disabled={category && !!resource}
				/>
				{category ? (
					<>
						<CostJSON
							label="Rules (JSON)"
							value={rules}
							onChange={setRules}
							description="1–500 REGULAR or INHERITED_VALUE rules. Definitions are stored without evaluation."
						/>
						<CostInput
							label="Default value (optional)"
							value={fallback}
							onChange={setFallback}
						/>
					</>
				) : monitor ? (
					!resource && (
						<>
							<CostSelect
								label="Monitor type"
								value={type}
								options={["DIMENSIONAL", "CUSTOM"]}
								onChange={setType}
							/>
							{type === "DIMENSIONAL" && (
								<CostSelect
									label="Dimension"
									value={dimension}
									options={[
										"SERVICE",
										"LINKED_ACCOUNT",
										"TAG",
										"COST_CATEGORY",
									]}
									onChange={setDimension}
								/>
							)}
							{(type === "CUSTOM" ||
								["TAG", "COST_CATEGORY"].includes(dimension)) && (
								<CostJSON
									label="Monitor specification (JSON)"
									value={spec}
									onChange={setSpec}
									description="CUSTOM: LINKED_ACCOUNT or Tags with 1–10 values, or CostCategories with one value. TAG/COST_CATEGORY dimensional: only the corresponding key."
								/>
							)}
						</>
					)
				) : (
					<>
						<CostSelect
							label="Frequency"
							value={frequency}
							options={["DAILY", "WEEKLY", "IMMEDIATE"]}
							onChange={setFrequency}
						/>
						<CostJSON
							label="Monitor ARNs (one per line)"
							value={arns}
							onChange={setArns}
						/>
						<CostJSON
							label="Threshold expression (JSON)"
							value={threshold}
							onChange={setThreshold}
							description="And/Or expressions over absolute or percentage anomaly impact; GREATER_THAN_OR_EQUAL with one nonnegative value."
						/>
						<CostJSON
							label="Subscriber addresses (one per line)"
							value={addresses}
							onChange={setAddresses}
							description="IMMEDIATE uses SNS topic ARNs; DAILY and WEEKLY use email addresses."
						/>
					</>
				)}
			</SpaceBetween>
		</ActionDialog>
	);
}
