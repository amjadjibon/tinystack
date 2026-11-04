import { HOTSPOT } from "./hotspots.ts";
import type { Tutorial } from "./types.ts";
export const s3FirstBucket: Tutorial = {
	id: "s3-first-bucket",
	title: "Store and retrieve a file in S3",
	description:
		"Create a bucket, upload a file, and download it. Learn how objects live inside buckets.",
	services: ["s3"],
	duration: "5 min",
	prerequisites:
		"Requires s3:CreateBucket, PutObject, GetObject and console listing permissions; cleanup requires DeleteObject and DeleteBucket. Use the generated names, keep versioning off, and upload a file named hello.txt.",
	learnMore: "https://amjadjibon.github.io/tinystack/guides/console-tutorials/",
	cleanup: async ({ names: n }, call) => [
		{
			label: `Object s3://${n.bucket}/${n.key}`,
			run: () => call("s3", "DeleteObject", { Bucket: n.bucket, Key: n.key }),
		},
		{
			label: `Bucket ${n.bucket} (must otherwise be empty)`,
			run: () => call("s3", "DeleteBucket", { Bucket: n.bucket }),
		},
	],
	tasks: [
		{
			title: "Store and retrieve a file",
			steps: [
				{
					title: "Create a bucket",
					content:
						"Choose Create bucket and name it {bucket}. Leave versioning disabled. A bucket groups your objects in this Region.",
					command: "aws-local s3api create-bucket --bucket {bucket}",
					hotspotId: HOTSPOT.s3Create,
					route: "/services/s3",
					check: "bucket",
				},
				{
					title: "Upload hello.txt",
					content:
						"Open {bucket}, choose Upload, select a local hello.txt file, and confirm Upload. Object keys identify files inside a bucket. The CLI command creates an empty hello.txt without needing a host file.",
					command: "aws-local s3api put-object --bucket {bucket} --key {key}",
					hotspotId: HOTSPOT.s3Upload,
					route: "/services/s3?bucket={bucket}",
					check: "object",
				},
				{
					title: "Download the object",
					content:
						"Open hello.txt and choose Download. GetObject retrieves the stored bytes without making the bucket public.",
					command:
						"aws-local s3api get-object --bucket {bucket} --key {key} /tmp/tutorial-download.txt",
					hotspotId: HOTSPOT.s3Download,
					route: "/services/s3?bucket={bucket}&key={key}",
					check: "download",
				},
			],
		},
	],
};
