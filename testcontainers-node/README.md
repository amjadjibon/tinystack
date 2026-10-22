# TinyStack Testcontainers for Node.js

Node.js / TypeScript [Testcontainers](https://node.testcontainers.org/) integration for [TinyStack](https://github.com/amjadjibon/tinystack), the lightweight local AWS emulator.

## Requirements

- Node.js 18+ (tested on Node 20, 22, 26)
- Docker-compatible container runtime

## Installation

```bash
npm install --save-dev testcontainers-tinystack testcontainers @aws-sdk/client-s3
```

## Quick Start

```typescript
import { TinyStackContainer } from "testcontainers-tinystack";
import { S3Client, CreateBucketCommand } from "@aws-sdk/client-s3";

// Start container
const container = await new TinyStackContainer().start();

// Configure AWS SDK v3 client using helper:
const s3 = new S3Client({
  ...container.getClientConfig(),
  forcePathStyle: true,
});

// Perform operations against TinyStack
await s3.send(new CreateBucketCommand({ Bucket: "my-test-bucket" }));

// Reset emulator state between tests without restart
await container.reset();

// Stop container when done
await container.stop();
```

## Configuration & Init Scripts

```typescript
const container = await new TinyStackContainer()
  .withConfig("test-config.yaml")
  .withInitScripts("ready", "init-data.sh")
  .withReadyScriptsComplete()
  .start();
```

## Resetting State

```typescript
// Reset all AWS services state
await container.reset();

// Reset state and re-execute ready-stage init scripts
await container.reset(true);
```

## Running Tests

```bash
npm test
```
