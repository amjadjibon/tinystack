import assert from "node:assert/strict";
import test from "node:test";
import {
	behaviorRows,
	cachePolicyName,
	callerReference,
	createDistributionInput,
	createInvalidationInput,
	createKvsInput,
	type DistributionConfig,
	type DistributionForm,
	distributionConfig,
	distributionForm,
	distributionStatusType,
	emptyDistributionForm,
	emptyOacForm,
	emptyOrigin,
	invalidationPaths,
	kvsComment,
	kvsName,
	kvsStatusType,
	methodsKey,
	oacConfig,
	originRows,
	originsText,
	parseAliases,
	tagItems,
	tagMap,
} from "../src/lib/cloudfrontHelpers.ts";
import { isoTime } from "../src/lib/format.ts";

const cachingOptimized = "658327ea-f89d-4fab-a63d-7e88639e58f6";

const form: DistributionForm = {
	...emptyDistributionForm(),
	comment: "  Web  ",
	aliases: "www.example.com\ncdn.example.com",
	origins: [
		{
			...emptyOrigin("s3"),
			domain: "assets.s3.us-east-1.amazonaws.com",
			accessControl: "E1OAC",
		},
		{
			...emptyOrigin("custom"),
			id: "api",
			domain: "api.example.com",
			protocol: "http-only",
			httpPort: "8080",
		},
	],
	target: "api",
	tags: [{ key: "team", value: "web" }],
};

const created: DistributionConfig = {
	CallerReference: "console-1",
	Comment: "Web",
	Enabled: true,
	Aliases: { Quantity: 2, Items: ["www.example.com", "cdn.example.com"] },
	DefaultRootObject: "",
	Origins: {
		Quantity: 2,
		Items: [
			{
				Id: "assets.s3.us-east-1.amazonaws.com",
				DomainName: "assets.s3.us-east-1.amazonaws.com",
				S3OriginConfig: { OriginAccessIdentity: "" },
				OriginAccessControlId: "E1OAC",
			},
			{
				Id: "api",
				DomainName: "api.example.com",
				CustomOriginConfig: {
					HTTPPort: 8080,
					HTTPSPort: 443,
					OriginProtocolPolicy: "http-only",
				},
			},
		],
	},
	DefaultCacheBehavior: {
		TargetOriginId: "api",
		ViewerProtocolPolicy: "redirect-to-https",
		AllowedMethods: {
			Quantity: 2,
			Items: ["GET", "HEAD"],
			CachedMethods: { Quantity: 2, Items: ["GET", "HEAD"] },
		},
		Compress: true,
		CachePolicyId: cachingOptimized,
	},
};

test("a new distribution carries origins, the default behavior and tags", () => {
	assert.deepEqual(createDistributionInput(form, "console-1"), {
		DistributionConfigWithTags: {
			DistributionConfig: created,
			Tags: { Items: [{ Key: "team", Value: "web" }] },
		},
	});
});

test("the only origin is the default behavior's target, and no tags send an empty list", () => {
	const input = createDistributionInput(
		{
			...emptyDistributionForm(),
			origins: [{ ...emptyOrigin(), domain: "bucket.s3.amazonaws.com" }],
		},
		"console-2",
	);
	const config = input.DistributionConfigWithTags.DistributionConfig;
	assert.equal(
		config.DefaultCacheBehavior?.TargetOriginId,
		"bucket.s3.amazonaws.com",
	);
	assert.equal(config.Origins?.Items?.[0].Id, "bucket.s3.amazonaws.com");
	assert.equal(config.Comment, "");
	assert.deepEqual(config.Aliases, { Quantity: 0 });
	assert.deepEqual(input.DistributionConfigWithTags.Tags, { Items: [] });
});

test("a distribution form rejects what CloudFront would refuse", () => {
	const build = (patch: Partial<DistributionForm>) =>
		distributionConfig({ CallerReference: "r" }, { ...form, ...patch });
	assert.throws(() => build({ origins: [] }), /at least one origin/);
	assert.throws(
		() => build({ origins: [{ ...emptyOrigin(), domain: "" }] }),
		/domain name/,
	);
	assert.throws(
		() =>
			build({
				origins: [{ ...emptyOrigin(), domain: "https://a.example.com" }],
			}),
		/without a scheme/,
	);
	assert.throws(
		() =>
			build({
				origins: [
					{ ...emptyOrigin(), domain: "a.example.com", path: "assets" },
				],
			}),
		/start with a slash/,
	);
	assert.throws(
		() =>
			build({
				origins: [
					{ ...emptyOrigin(), domain: "a.example.com", path: "/assets/" },
				],
			}),
		/not end with one/,
	);
	assert.throws(
		() =>
			build({
				origins: [
					{ ...emptyOrigin("custom"), domain: "a.example.com", httpPort: "0" },
				],
			}),
		/HTTP port/,
	);
	assert.throws(
		() =>
			build({
				origins: [
					{
						...emptyOrigin("custom"),
						domain: "a.example.com",
						protocol: "ftp",
					},
				],
			}),
		/protocol policy/,
	);
	assert.throws(
		() => build({ origins: [form.origins[1], form.origins[1]] }),
		/Origin ID api is used twice/,
	);
	assert.throws(() => build({ target: "missing" }), /Choose the origin/);
	assert.throws(() => build({ target: "" }), /Choose the origin/);
	assert.throws(() => build({ viewerProtocol: "ftp" }), /viewer protocol/);
	assert.throws(() => build({ methods: "none" }), /allowed HTTP methods/);
	assert.throws(() => build({ comment: "x".repeat(129) }), /128 characters/);
	assert.throws(() => build({ defaultRootObject: "/index.html" }), /slash/);
	assert.throws(() => build({ cachePolicyId: "a b" }), /spaces/);
	assert.throws(() => build({ aliases: "nodots" }), /not a valid alternate/);
	const withTags = (tags: DistributionForm["tags"]) => () =>
		createDistributionInput({ ...form, tags }, "r");
	assert.throws(withTags([{ key: "", value: "orphan" }]), /needs a key/);
	assert.throws(
		withTags([
			{ key: "team", value: "a" },
			{ key: "team", value: "b" },
		]),
		/repeated/,
	);
});

test("alternate domain names are validated and unique", () => {
	assert.deepEqual(parseAliases("a.example.com, *.example.com\n"), [
		"a.example.com",
		"*.example.com",
	]);
	assert.throws(() => parseAliases("A.example.com\na.example.com"), /unique/);
	assert.throws(() => parseAliases("bad_name.example.com"), /not a valid/);
});

test("the form of a configuration builds that configuration again", () => {
	assert.deepEqual(
		distributionConfig(created, distributionForm(created)),
		created,
	);
});

test("editing keeps the settings the form does not show", () => {
	const existing: DistributionConfig = {
		...created,
		ViewerCertificate: { CloudFrontDefaultCertificate: true },
		CustomErrorResponses: { Quantity: 0 },
		CacheBehaviors: {
			Quantity: 1,
			Items: [{ PathPattern: "/api/*", TargetOriginId: "api", MinTTL: 5 }],
		},
		DefaultCacheBehavior: {
			...created.DefaultCacheBehavior,
			AllowedMethods: {
				Quantity: 3,
				Items: ["GET", "HEAD", "OPTIONS"],
				CachedMethods: { Quantity: 3, Items: ["GET", "HEAD", "OPTIONS"] },
			},
			SmoothStreaming: true,
		},
		Origins: {
			Quantity: 2,
			Items: [
				{
					...created.Origins?.Items?.[0],
					OriginShield: { Enabled: false },
					S3OriginConfig: { OriginAccessIdentity: "", OriginReadTimeout: 30 },
				},
				{
					...created.Origins?.Items?.[1],
					CustomHeaders: { Quantity: 1, Items: [{ HeaderName: "x" }] },
					ConnectionAttempts: 2,
				},
			],
		},
	};
	const edited = distributionForm(existing);
	edited.comment = "Changed";
	edited.origins[1].httpsPort = "8443";
	const config = distributionConfig(existing, edited);
	assert.equal(config.Comment, "Changed");
	assert.equal(config.CallerReference, "console-1");
	assert.deepEqual(config.ViewerCertificate, {
		CloudFrontDefaultCertificate: true,
	});
	assert.deepEqual(config.CustomErrorResponses, { Quantity: 0 });
	assert.deepEqual(config.CacheBehaviors, existing.CacheBehaviors);
	assert.equal(config.DefaultCacheBehavior?.SmoothStreaming, true);
	assert.deepEqual(config.DefaultCacheBehavior?.AllowedMethods, {
		Quantity: 3,
		Items: ["GET", "HEAD", "OPTIONS"],
		CachedMethods: { Quantity: 3, Items: ["GET", "HEAD", "OPTIONS"] },
	});
	assert.deepEqual(config.Origins?.Items?.[0].OriginShield, { Enabled: false });
	assert.equal(
		config.Origins?.Items?.[0].S3OriginConfig?.OriginReadTimeout,
		30,
	);
	assert.equal(config.Origins?.Items?.[1].ConnectionAttempts, 2);
	assert.deepEqual(config.Origins?.Items?.[1].CustomHeaders, {
		Quantity: 1,
		Items: [{ HeaderName: "x" }],
	});
	assert.equal(config.Origins?.Items?.[1].CustomOriginConfig?.HTTPSPort, 8443);
	// Narrowing the allowed methods drops cached methods that are no longer allowed.
	edited.methods = "read";
	assert.deepEqual(
		distributionConfig(existing, edited).DefaultCacheBehavior?.AllowedMethods,
		created.DefaultCacheBehavior?.AllowedMethods,
	);
});

test("an origin access control replaces an origin access identity", () => {
	const legacy: DistributionConfig = {
		...created,
		Origins: {
			Quantity: 1,
			Items: [
				{
					Id: "assets",
					DomainName: "assets.s3.amazonaws.com",
					S3OriginConfig: {
						OriginAccessIdentity: "origin-access-identity/cloudfront/E1",
					},
				},
			],
		},
		DefaultCacheBehavior: {
			...created.DefaultCacheBehavior,
			TargetOriginId: "assets",
		},
	};
	const edited = distributionForm(legacy);
	assert.equal(
		distributionConfig(legacy, edited).Origins?.Items?.[0].S3OriginConfig
			?.OriginAccessIdentity,
		"origin-access-identity/cloudfront/E1",
	);
	edited.origins[0].accessControl = "E2OAC";
	const origin = distributionConfig(legacy, edited).Origins?.Items?.[0];
	assert.equal(origin?.S3OriginConfig?.OriginAccessIdentity, "");
	assert.equal(origin?.OriginAccessControlId, "E2OAC");
	edited.origins[0].accessControl = "";
	assert.equal(
		distributionConfig(legacy, edited).Origins?.Items?.[0]
			.OriginAccessControlId,
		undefined,
	);
});

test("origins of other types pass through, and behaviors may target origin groups", () => {
	const existing: DistributionConfig = {
		...created,
		Origins: {
			Quantity: 1,
			Items: [
				{
					Id: "vpc",
					DomainName: "vpc.example.com",
					VpcOriginConfig: { VpcOriginId: "v" },
				},
			],
		},
		OriginGroups: { Quantity: 1, Items: [{ Id: "group" }] },
		DefaultCacheBehavior: {
			...created.DefaultCacheBehavior,
			TargetOriginId: "group",
		},
	};
	const edited = distributionForm(existing);
	assert.equal(edited.origins[0].kind, "other");
	const config = distributionConfig(existing, edited);
	assert.deepEqual(config.Origins, existing.Origins);
	assert.equal(config.DefaultCacheBehavior?.TargetOriginId, "group");
	assert.throws(
		() => distributionConfig({ CallerReference: "r" }, edited),
		/cannot be added here/,
	);
});

test("an origin that a cache behavior still uses cannot be removed", () => {
	const existing: DistributionConfig = {
		...created,
		CacheBehaviors: {
			Quantity: 1,
			Items: [
				{
					PathPattern: "/assets/*",
					TargetOriginId: "assets.s3.us-east-1.amazonaws.com",
				},
			],
		},
	};
	const edited = distributionForm(existing);
	edited.origins = [edited.origins[1]];
	assert.throws(
		() => distributionConfig(existing, edited),
		/cache behavior \/assets\/\* sends requests to origin assets/,
	);
});

test("allowed methods are one of the three sets CloudFront permits", () => {
	assert.equal(methodsKey(["HEAD", "GET"]), "read");
	assert.equal(methodsKey(["GET", "HEAD", "OPTIONS"]), "options");
	assert.equal(
		methodsKey(["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"]),
		"all",
	);
	assert.equal(methodsKey(undefined), "read");
});

test("caller references are readable and unique per call", () => {
	assert.equal(
		callerReference("console", 1700000000000, "abc123"),
		"console-1700000000000-abc123",
	);
	assert.notEqual(callerReference("console"), callerReference("console"));
});

test("invalidation paths are trimmed, unique and checked", () => {
	assert.deepEqual(
		invalidationPaths(" /index.html \n\n/images/*\n/index.html"),
		["/index.html", "/images/*"],
	);
	assert.throws(() => invalidationPaths(" \n"), /at least one path/);
	assert.throws(() => invalidationPaths("index.html"), /start with a slash/);
	assert.throws(() => invalidationPaths("/a*/b"), /asterisk can only end/);
	assert.throws(
		() =>
			invalidationPaths(
				Array.from({ length: 3001 }, (_, i) => `/p${i}`).join("\n"),
			),
		/at most 3000/,
	);
	assert.deepEqual(createInvalidationInput("E1", "/a\n/b/*", "console-9"), {
		DistributionId: "E1",
		InvalidationBatch: {
			CallerReference: "console-9",
			Paths: { Quantity: 2, Items: ["/a", "/b/*"] },
		},
	});
});

test("origin access controls need a name and a known type", () => {
	assert.deepEqual(
		oacConfig({ ...emptyOacForm, name: " assets ", description: " " }),
		{
			Name: "assets",
			OriginAccessControlOriginType: "s3",
			SigningBehavior: "always",
			SigningProtocol: "sigv4",
		},
	);
	assert.deepEqual(
		oacConfig({
			...emptyOacForm,
			name: "a",
			description: "d",
			signingBehavior: "never",
		}),
		{
			Name: "a",
			Description: "d",
			OriginAccessControlOriginType: "s3",
			SigningBehavior: "never",
			SigningProtocol: "sigv4",
		},
	);
	assert.throws(() => oacConfig(emptyOacForm), /1 to 64/);
	assert.throws(
		() => oacConfig({ ...emptyOacForm, name: "x".repeat(65) }),
		/1 to 64/,
	);
	assert.throws(
		() =>
			oacConfig({ ...emptyOacForm, name: "a", description: "d".repeat(257) }),
		/256/,
	);
	assert.throws(
		() => oacConfig({ ...emptyOacForm, name: "a", originType: "ftp" }),
		/origin type/,
	);
	assert.throws(
		() => oacConfig({ ...emptyOacForm, name: "a", signingBehavior: "" }),
		/signing behavior/,
	);
});

test("key value stores take a short name and a comment", () => {
	assert.equal(kvsName(" redirects_1 "), "redirects_1");
	assert.throws(() => kvsName("has space"), /1 to 64/);
	assert.throws(() => kvsName(""), /1 to 64/);
	assert.throws(() => kvsComment("x".repeat(129)), /128/);
	assert.deepEqual(createKvsInput({ name: "s", comment: " c ", tags: [] }), {
		Name: "s",
		Comment: "c",
	});
	assert.deepEqual(
		createKvsInput({
			name: "s",
			comment: "",
			tags: [{ key: "k", value: "v" }],
		}),
		{ Name: "s", Comment: "", Tags: { Items: [{ Key: "k", Value: "v" }] } },
	);
	assert.equal(kvsStatusType("READY"), "success");
	assert.equal(kvsStatusType("PROVISIONING"), "in-progress");
	assert.equal(kvsStatusType("IMPORT_FAILED"), "error");
});

test("tags convert between maps and CloudFront lists", () => {
	assert.deepEqual(tagItems({ a: "1", b: "2" }), {
		Items: [
			{ Key: "a", Value: "1" },
			{ Key: "b", Value: "2" },
		],
	});
	assert.deepEqual(
		tagMap({ Items: [{ Key: "a", Value: "1" }, { Key: "b" }] }),
		{
			a: "1",
			b: "",
		},
	);
	assert.deepEqual(tagMap(undefined), {});
});

test("a distribution is summarized by its origins and behaviors", () => {
	assert.equal(
		originsText(created),
		"assets.s3.us-east-1.amazonaws.com, api.example.com",
	);
	assert.deepEqual(
		originRows(created).map((o) => [o.id, o.kind, o.accessControl]),
		[
			["assets.s3.us-east-1.amazonaws.com", "S3", "E1OAC"],
			["api", "Custom", "-"],
		],
	);
	const rows = behaviorRows({
		...created,
		CacheBehaviors: {
			Quantity: 1,
			Items: [{ PathPattern: "/api/*", TargetOriginId: "api" }],
		},
	});
	assert.deepEqual(rows[0], {
		path: "Default (*)",
		target: "api",
		viewerProtocol: "redirect-to-https",
		methods: "GET, HEAD",
		compress: "Yes",
		cachePolicy: "Managed-CachingOptimized",
	});
	assert.equal(rows[1].path, "/api/*");
	assert.equal(rows[1].cachePolicy, "-");
	assert.deepEqual(behaviorRows(undefined), []);
	assert.equal(cachePolicyName("custom-id"), "custom-id");
	assert.equal(distributionStatusType("Deployed"), "success");
	assert.equal(distributionStatusType("InProgress"), "in-progress");
});

test("timestamps from console actions show in the viewer's locale", () => {
	assert.equal(isoTime(undefined), "-");
	assert.equal(isoTime(""), "-");
	assert.equal(
		isoTime("2026-11-02T10:20:30.123456789Z"),
		new Date("2026-11-02T10:20:30.123Z").toLocaleString(),
	);
	assert.equal(
		isoTime("2026-11-02T10:20:30Z"),
		new Date("2026-11-02T10:20:30Z").toLocaleString(),
	);
	assert.equal(isoTime("not a time"), "not a time");
});
