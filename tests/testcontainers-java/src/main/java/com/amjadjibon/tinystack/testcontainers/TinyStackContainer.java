package com.amjadjibon.tinystack.testcontainers;

import org.testcontainers.containers.GenericContainer;
import org.testcontainers.containers.wait.strategy.Wait;
import org.testcontainers.containers.wait.strategy.WaitAllStrategy;
import org.testcontainers.containers.wait.strategy.WaitStrategy;
import org.testcontainers.utility.DockerImageName;
import org.testcontainers.utility.MountableFile;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.time.Duration;
import java.util.HashSet;
import java.util.Set;

/**
 * TinyStack container for Testcontainers Java integration testing.
 */
public class TinyStackContainer extends GenericContainer<TinyStackContainer> {

    public static final int AWS_PORT = 4566;
    public static final DockerImageName DEFAULT_IMAGE_NAME =
            DockerImageName.parse("ghcr.io/amjadjibon/tinystack:latest");

    private static final Set<String> VALID_STAGES = Set.of("boot", "start", "ready", "shutdown");
    private final Set<String> initScriptDestinations = new HashSet<>();

    public TinyStackContainer() {
        this(resolveDefaultImage());
    }

    public TinyStackContainer(String dockerImageName) {
        this(DockerImageName.parse(dockerImageName));
    }

    public TinyStackContainer(DockerImageName dockerImageName) {
        super(dockerImageName);
        withExposedPorts(AWS_PORT);
        withCommand("start", "--host", "0.0.0.0", "--port", String.valueOf(AWS_PORT));
        waitingFor(Wait.forHttp("/_tinystack/health")
                .forPort(AWS_PORT)
                .forStatusCode(200)
                .withStartupTimeout(Duration.ofSeconds(120)));
    }

    private static DockerImageName resolveDefaultImage() {
        String envImage = System.getenv("TINYSTACK_IMAGE");
        if (envImage != null && !envImage.trim().isEmpty()) {
            return DockerImageName.parse(envImage.trim());
        }
        return DEFAULT_IMAGE_NAME;
    }

    /**
     * Return the host-accessible AWS endpoint URL including the HTTP scheme.
     */
    public String getEndpointUrl() {
        return "http://" + getHost() + ":" + getMappedPort(AWS_PORT);
    }

    /**
     * Alias for {@link #getEndpointUrl()}.
     */
    public String getEndpoint() {
        return getEndpointUrl();
    }

    /**
     * Return the endpoint URI (convenient for AWS SDK v2 client endpoint override).
     */
    public URI getEndpointOverride() {
        return URI.create(getEndpointUrl());
    }

    /**
     * Return the default region.
     */
    public String getRegion() {
        return "us-east-1";
    }

    /**
     * Return dummy access key.
     */
    public String getAccessKey() {
        return "test";
    }

    /**
     * Return dummy secret key.
     */
    public String getSecretKey() {
        return "test";
    }

    /**
     * Clear TinyStack's state via POST /_tinystack/reset without re-running ready scripts.
     */
    public void reset() {
        reset(false);
    }

    /**
     * Clear TinyStack's state via POST /_tinystack/reset.
     *
     * @param init if true, ready-stage init scripts are re-run (?init=1).
     */
    public void reset(boolean init) {
        String endpoint = getEndpointUrl();
        String path = init ? "/_tinystack/reset?init=1" : "/_tinystack/reset";
        try {
            HttpClient client = HttpClient.newBuilder()
                    .connectTimeout(Duration.ofSeconds(10))
                    .build();
            HttpRequest request = HttpRequest.newBuilder()
                    .uri(URI.create(endpoint + path))
                    .timeout(Duration.ofSeconds(60))
                    .POST(HttpRequest.BodyPublishers.noBody())
                    .build();
            HttpResponse<String> response = client.send(request, HttpResponse.BodyHandlers.ofString());
            if (response.statusCode() < 200 || response.statusCode() >= 300) {
                throw new RuntimeException("reset tinystack: " + response.statusCode() + ": " + response.body());
            }
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new RuntimeException("reset tinystack interrupted", e);
        } catch (IOException e) {
            throw new RuntimeException("reset tinystack failed: " + e.getMessage(), e);
        }
    }

    /**
     * Copy a YAML configuration to /etc/tinystack/config.yaml and append --config to the startup command.
     */
    public TinyStackContainer withConfig(String hostFilePath) {
        withCopyFileToContainer(MountableFile.forHostPath(hostFilePath), "/etc/tinystack/config.yaml");
        withCommand("start", "--host", "0.0.0.0", "--port", String.valueOf(AWS_PORT), "--config", "/etc/tinystack/config.yaml");
        return this;
    }

    /**
     * Copy a YAML configuration to /etc/tinystack/config.yaml and append --config to the startup command.
     */
    public TinyStackContainer withConfig(Path hostFilePath) {
        return withConfig(hostFilePath.toAbsolutePath().toString());
    }

    /**
     * Copy local files into /etc/tinystack/init/&lt;stage&gt;.d, preserving permissions.
     * Stages are boot, start, ready, and shutdown.
     */
    public TinyStackContainer withInitScripts(String stage, String... files) {
        if (!VALID_STAGES.contains(stage)) {
            throw new IllegalArgumentException("unsupported init stage: " + stage);
        }
        for (String file : files) {
            Path path = Paths.get(file);
            if (!Files.isRegularFile(path)) {
                throw new IllegalArgumentException("init script " + file + " is not a regular file");
            }
            String destination = "/etc/tinystack/init/" + stage + ".d/" + path.getFileName().toString();
            if (initScriptDestinations.contains(destination)) {
                throw new IllegalArgumentException("duplicate init script destination: " + destination);
            }
            initScriptDestinations.add(destination);
            withCopyFileToContainer(MountableFile.forHostPath(path.toAbsolutePath().toString(), 0755), destination);
        }
        return this;
    }

    /**
     * Copy local files into /etc/tinystack/init/&lt;stage&gt;.d, preserving permissions.
     */
    public TinyStackContainer withInitScripts(String stage, Path... files) {
        String[] paths = new String[files.length];
        for (int i = 0; i < files.length; i++) {
            paths[i] = files[i].toAbsolutePath().toString();
        }
        return withInitScripts(stage, paths);
    }

    /**
     * Add a wait for the ready stage to finish successfully (completed=true and state=SUCCESSFUL).
     */
    public TinyStackContainer withReadyScriptsComplete() {
        WaitStrategy readyWait = Wait.forHttp("/_tinystack/init/ready")
                .forPort(AWS_PORT)
                .forResponsePredicate(body -> body != null && body.contains("\"completed\":true") && body.contains("\"state\":\"SUCCESSFUL\""))
                .withStartupTimeout(Duration.ofSeconds(120));
        waitingFor(new WaitAllStrategy()
                .withStrategy(Wait.forHttp("/_tinystack/health").forPort(AWS_PORT).forStatusCode(200).withStartupTimeout(Duration.ofSeconds(120)))
                .withStrategy(readyWait));
        return this;
    }
}
