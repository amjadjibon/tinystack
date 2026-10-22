import assert from "node:assert";
import { execSync } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test, { after, before, describe } from "node:test";
import {
  CreateBucketCommand,
  ListBucketsCommand,
  S3Client,
} from "@aws-sdk/client-s3";
import {
  CreateQueueCommand,
  ListQueuesCommand,
  SQSClient,
} from "@aws-sdk/client-sqs";
import { StartedTinyStackContainer, TinyStackContainer } from "../dist/index.js";

function resolveOrCreateImage(): { image: string; createdTag?: string } {
  const envImage = process.env.TINYSTACK_IMAGE;
  if (envImage && envImage.trim()) {
    return { image: envImage.trim() };
  }
  const tag = `tinystack-test-node:${Date.now()}`;
  const repoRoot = path.resolve("..");
  execSync(`docker build --target headless -t ${tag} ${repoRoot}`, {
    stdio: "inherit",
  });
  return { image: tag, createdTag: tag };
}

describe("TinyStackContainer Node", () => {
  let container: StartedTinyStackContainer;
  let createdTag: string | undefined;
  let s3: S3Client;
  let sqs: SQSClient;

  before(async () => {
    const resolved = resolveOrCreateImage();
    createdTag = resolved.createdTag;

    container = await new TinyStackContainer(resolved.image).start();

    const config = container.getClientConfig();
    s3 = new S3Client({
      ...config,
      forcePathStyle: true,
    });
    sqs = new SQSClient(config);
  });

  after(async () => {
    s3?.destroy();
    sqs?.destroy();
    if (container) {
      await container.stop();
    }
    if (createdTag) {
      try {
        execSync(`docker image rm ${createdTag}`, { stdio: "ignore" });
      } catch {
        // ignore
      }
    }
  });

  test("health endpoint returns 200 with status ok", async () => {
    const res = await fetch(`${container.getEndpointUrl()}/_tinystack/health`);
    assert.strictEqual(res.status, 200);
    const body = (await res.json()) as { status: string; services: unknown; storage: unknown };
    assert.strictEqual(body.status, "ok");
    assert.ok(body.services);
    assert.ok(body.storage);
  });

  test("S3 and SQS operations work and reset clears state", async () => {
    const bucketName = "testcontainers-node-bucket";
    const queueName = "testcontainers-node-queue";

    await s3.send(new CreateBucketCommand({ Bucket: bucketName }));
    const bucketsBefore = await s3.send(new ListBucketsCommand({}));
    const bucketNames = (bucketsBefore.Buckets || []).map((b) => b.Name);
    assert.ok(bucketNames.includes(bucketName));

    const queueRes = await sqs.send(new CreateQueueCommand({ QueueName: queueName }));
    const queuesBefore = await sqs.send(new ListQueuesCommand({}));
    assert.ok((queuesBefore.QueueUrls || []).includes(queueRes.QueueUrl!));

    await container.reset();

    const bucketsAfter = await s3.send(new ListBucketsCommand({}));
    const bucketNamesAfter = (bucketsAfter.Buckets || []).map((b) => b.Name);
    assert.strictEqual(bucketNamesAfter.includes(bucketName), false);

    const queuesAfter = await sqs.send(new ListQueuesCommand({}));
    assert.strictEqual((queuesAfter.QueueUrls || []).includes(queueRes.QueueUrl!), false);
  });

  test("reset with init parameter", async () => {
    await container.reset(true);
  });

  test("options validation", () => {
    const tempFile = path.join(os.tmpdir(), `tinystack-script-${Date.now()}.sh`);
    fs.writeFileSync(tempFile, "#!/bin/sh\necho ok\n");

    try {
      const custom = new TinyStackContainer();
      assert.throws(() => custom.withInitScripts("invalid-stage", tempFile), /unsupported init stage/);
      assert.throws(() => custom.withInitScripts("ready", "/non/existent/file.sh"), /not a regular file/);

      custom.withInitScripts("ready", tempFile);
      assert.throws(() => custom.withInitScripts("ready", tempFile), /duplicate init script destination/);
    } finally {
      if (fs.existsSync(tempFile)) {
        fs.unlinkSync(tempFile);
      }
    }
  });

  test("reset throws on HTTP error", async () => {
    const server = http.createServer((_, res) => {
      res.writeHead(500, { "Content-Type": "text/plain" });
      res.end("mock server error");
    });

    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", () => resolve()));
    const address = server.address() as { port: number };

    const mockStarted = Object.create(container) as StartedTinyStackContainer;
    mockStarted.getEndpointUrl = () => `http://127.0.0.1:${address.port}`;

    try {
      await assert.rejects(
        async () => {
          await mockStarted.reset();
        },
        /reset tinystack: 500/
      );
    } finally {
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  });
});
