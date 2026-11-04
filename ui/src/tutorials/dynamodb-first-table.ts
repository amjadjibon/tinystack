import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const dynamodbFirstTable: Tutorial = {
	id: "dynamodb-first-table",
	title: "Store and read items in DynamoDB",
	description:
		"Create a table with a partition key, write an item, and read it back. Learn how keys identify items.",
	services: ["dynamodb"],
	duration: "5 min",
	prerequisites:
		"Requires dynamodb:CreateTable, PutItem, GetItem, Query or Scan and console listing permissions; cleanup requires DeleteTable. Use the generated table name and keep the partition key pk (String).",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	cleanup: async ({ names: n }, call) => [
		{
			label: `Table ${n.table} and all its items`,
			run: () => call("dynamodb", "DeleteTable", { TableName: n.table }),
		},
	],
	tasks: [
		{
			title: "Write and read an item",
			steps: [
				{
					title: "Create a table",
					content:
						"Choose Create table, name it {table}, and keep the partition key pk of type String. On-demand capacity needs no throughput settings. Every item is identified by its key.",
					command:
						"aws-local dynamodb create-table --table-name {table} --attribute-definitions AttributeName=pk,AttributeType=S --key-schema AttributeName=pk,KeyType=HASH --billing-mode PAY_PER_REQUEST",
					hotspotId: HOTSPOT.dynamoCreate,
					route: "/services/dynamodb",
					check: "table",
				},
				{
					title: "Write an item",
					content:
						'Open {table}, choose Create item, and save {"pk": {"S": "hello"}, "message": {"S": "Hello TinyStack"}}. Items need only their key; other attributes can differ from item to item.',
					command:
						'aws-local dynamodb put-item --table-name {table} --item \'{"pk":{"S":"hello"},"message":{"S":"Hello TinyStack"}}\'',
					hotspotId: HOTSPOT.dynamoCreateItem,
					route: "/services/dynamodb?table={table}",
					check: "item",
				},
				{
					title: "Read it back",
					content:
						"Keep Scan selected and choose Run, or choose Query with the condition pk = :pk. Returning at least one item completes the tutorial. Query reads one partition; Scan reads the whole table.",
					command:
						'aws-local dynamodb get-item --table-name {table} --key \'{"pk":{"S":"hello"}}\'',
					hotspotId: HOTSPOT.dynamoRun,
					route: "/services/dynamodb?table={table}",
					check: "read",
				},
			],
		},
	],
};
