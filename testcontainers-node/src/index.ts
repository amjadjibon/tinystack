import fs from "node:fs";
import path from "node:path";
import {
  AbstractStartedContainer,
  GenericContainer,
  type StartedTestContainer,
  Wait,
} from "testcontainers";

/**
 * TinyStack container for Testcontainers Node.
 */
export class TinyStackContainer extends GenericContainer {
  public static readonly AWS_PORT = 4566;
  public static readonly DEFAULT_IMAGE = "ghcr.io/amjadjibon/tinystack:latest";
  private static readonly VALID_STAGES = new Set(["boot", "start", "ready", "shutdown"]);

  private initScriptDestinations = new Set<string>();

  constructor(image?: string) {
    const resolvedImage = image || process.env.TINYSTACK_IMAGE || TinyStackContainer.DEFAULT_IMAGE;
    super(resolvedImage);
    this.withExposedPorts(TinyStackContainer.AWS_PORT);
    this.withCommand(["start", "--host", "0.0.0.0", "--port", String(TinyStackContainer.AWS_PORT)]);
    this.withWaitStrategy(
      Wait.forHttp("/_tinystack/health", TinyStackContainer.AWS_PORT)
        .forStatusCode(200)
        .withStartupTimeout(120_000)
    );
  }

  /**
   * Copy a YAML configuration to /etc/tinystack/config.yaml and append --config to startup command.
   */
  public withConfig(hostFilePath: string): this {
    const resolvedPath = path.resolve(hostFilePath);
    if (!fs.existsSync(resolvedPath) || !fs.statSync(resolvedPath).isFile()) {
      throw new Error(`config file "${hostFilePath}" is not a regular file`);
    }
    this.withCopyFilesToContainer([
      { source: resolvedPath, target: "/etc/tinystack/config.yaml", mode: 0o644 },
    ]);
    this.withCommand([
      "start",
      "--host",
      "0.0.0.0",
      "--port",
      String(TinyStackContainer.AWS_PORT),
      "--config",
      "/etc/tinystack/config.yaml",
    ]);
    return this;
  }

  /**
   * Copy local files into /etc/tinystack/init/<stage>.d, preserving permissions.
   * Stages are boot, start, ready, and shutdown.
   */
  public withInitScripts(stage: string, ...files: string[]): this {
    if (!TinyStackContainer.VALID_STAGES.has(stage)) {
      throw new Error(`unsupported init stage "${stage}"`);
    }
    const copies = files.map((file) => {
      const resolved = path.resolve(file);
      if (!fs.existsSync(resolved) || !fs.statSync(resolved).isFile()) {
        throw new Error(`init script "${file}" is not a regular file`);
      }
      const destination = `/etc/tinystack/init/${stage}.d/${path.basename(resolved)}`;
      if (this.initScriptDestinations.has(destination)) {
        throw new Error(`duplicate init script destination "${destination}"`);
      }
      this.initScriptDestinations.add(destination);
      return {
        source: resolved,
        target: destination,
        mode: 0o755,
      };
    });

    this.withCopyFilesToContainer(copies);
    return this;
  }

  /**
   * Add a wait strategy requiring GET /_tinystack/init/ready to return 200 with completed=true and state=SUCCESSFUL.
   */
  public withReadyScriptsComplete(): this {
    const healthWait = Wait.forHttp("/_tinystack/health", TinyStackContainer.AWS_PORT)
      .forStatusCode(200)
      .withStartupTimeout(120_000);
    const readyWait = Wait.forHttp("/_tinystack/init/ready", TinyStackContainer.AWS_PORT)
      .forStatusCode(200)
      .forResponsePredicate((response) => {
        try {
          const data = JSON.parse(response);
          return data.completed === true && data.state === "SUCCESSFUL";
        } catch {
          return false;
        }
      })
      .withStartupTimeout(120_000);

    this.withWaitStrategy(Wait.forAll([healthWait, readyWait]));
    return this;
  }

  public override async start(): Promise<StartedTinyStackContainer> {
    const started = await super.start();
    return new StartedTinyStackContainer(started);
  }
}

/**
 * Started TinyStack container instance with helper methods.
 */
export class StartedTinyStackContainer extends AbstractStartedContainer {
  constructor(startedTestContainer: StartedTestContainer) {
    super(startedTestContainer);
  }

  /**
   * Return the host-accessible AWS endpoint URL including the HTTP scheme.
   */
  public getEndpointUrl(): string {
    return `http://${this.getHost()}:${this.getMappedPort(TinyStackContainer.AWS_PORT)}`;
  }

  /**
   * Alias for getEndpointUrl.
   */
  public getEndpoint(): string {
    return this.getEndpointUrl();
  }

  /**
   * Return default region.
   */
  public getRegion(): string {
    return "us-east-1";
  }

  /**
   * Return dummy access key.
   */
  public getAccessKeyId(): string {
    return "test";
  }

  /**
   * Alias for getAccessKeyId.
   */
  public getAccessKey(): string {
    return this.getAccessKeyId();
  }

  /**
   * Return dummy secret key.
   */
  public getSecretAccessKey(): string {
    return "test";
  }

  /**
   * Alias for getSecretAccessKey.
   */
  public getSecretKey(): string {
    return this.getSecretAccessKey();
  }

  /**
   * Helper returning configuration options suitable for AWS SDK v3 JavaScript clients.
   */
  public getClientConfig(region = "us-east-1"): {
    endpoint: string;
    region: string;
    credentials: { accessKeyId: string; secretAccessKey: string };
  } {
    return {
      endpoint: this.getEndpointUrl(),
      region,
      credentials: {
        accessKeyId: this.getAccessKeyId(),
        secretAccessKey: this.getSecretAccessKey(),
      },
    };
  }

  /**
   * Clear TinyStack's state via POST /_tinystack/reset.
   *
   * @param init if true, ready-stage init scripts are re-run (?init=1).
   */
  public async reset(init = false): Promise<void> {
    const endpoint = this.getEndpointUrl();
    const url = `${endpoint}${init ? "/_tinystack/reset?init=1" : "/_tinystack/reset"}`;
    const response = await fetch(url, {
      method: "POST",
      signal: AbortSignal.timeout(60_000),
    });
    if (!response.ok) {
      const text = await response.text().catch(() => "");
      throw new Error(`reset tinystack: ${response.status} ${response.statusText}: ${text}`);
    }
  }
}
