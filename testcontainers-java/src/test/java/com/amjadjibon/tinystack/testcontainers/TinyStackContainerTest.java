package com.amjadjibon.tinystack.testcontainers;

import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.Assumptions;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Test;
import org.testcontainers.DockerClientFactory;
import software.amazon.awssdk.auth.credentials.AwsBasicCredentials;
import software.amazon.awssdk.auth.credentials.StaticCredentialsProvider;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.s3.S3Client;
import software.amazon.awssdk.services.s3.model.Bucket;
import software.amazon.awssdk.services.sqs.SqsClient;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.time.Duration;
import java.util.List;

import static org.junit.jupiter.api.Assertions.*;

public class TinyStackContainerTest {

    private static String builtImageTag;
    private static TinyStackContainer container;
    private static S3Client s3;
    private static SqsClient sqs;

    @BeforeAll
    public static void setUpAll() throws Exception {
        Assumptions.assumeTrue(
                DockerClientFactory.instance().isDockerAvailable(),
                "Docker is not available; skipping Docker-backed tests"
        );

        String image = resolveOrCreateImage();
        container = new TinyStackContainer(image);
        container.start();

        s3 = S3Client.builder()
                .endpointOverride(container.getEndpointOverride())
                .region(Region.of(container.getRegion()))
                .credentialsProvider(StaticCredentialsProvider.create(
                        AwsBasicCredentials.create(container.getAccessKey(), container.getSecretKey())))
                .forcePathStyle(true)
                .build();

        sqs = SqsClient.builder()
                .endpointOverride(container.getEndpointOverride())
                .region(Region.of(container.getRegion()))
                .credentialsProvider(StaticCredentialsProvider.create(
                        AwsBasicCredentials.create(container.getAccessKey(), container.getSecretKey())))
                .build();
    }

    @AfterAll
    public static void tearDownAll() {
        if (s3 != null) {
            s3.close();
        }
        if (sqs != null) {
            sqs.close();
        }
        if (container != null) {
            container.stop();
        }
        if (builtImageTag != null) {
            try {
                new ProcessBuilder("docker", "image", "rm", builtImageTag).start().waitFor();
            } catch (Exception ignored) {
            }
        }
    }

    private static String resolveOrCreateImage() throws Exception {
        String envImage = System.getenv("TINYSTACK_IMAGE");
        if (envImage != null && !envImage.trim().isEmpty()) {
            return envImage.trim();
        }

        Path repoRoot = Paths.get("..").toAbsolutePath().normalize();
        String tag = "tinystack-test:" + System.currentTimeMillis();
        ProcessBuilder pb = new ProcessBuilder("docker", "build", "--target", "headless", "-t", tag, repoRoot.toString());
        pb.inheritIO();
        Process process = pb.start();
        int exitCode = process.waitFor();
        if (exitCode != 0) {
            throw new RuntimeException("docker build failed with exit code: " + exitCode);
        }
        builtImageTag = tag;
        return tag;
    }

    @Test
    public void testHealthEndpoint() throws Exception {
        HttpClient client = HttpClient.newHttpClient();
        HttpRequest request = HttpRequest.newBuilder()
                .uri(URI.create(container.getEndpointUrl() + "/_tinystack/health"))
                .timeout(Duration.ofSeconds(10))
                .GET()
                .build();
        HttpResponse<String> response = client.send(request, HttpResponse.BodyHandlers.ofString());
        assertEquals(200, response.statusCode());
        assertTrue(response.body().contains("\"status\":\"ok\""));
        assertTrue(response.body().contains("\"services\""));
        assertTrue(response.body().contains("\"storage\""));
    }

    @Test
    public void testS3SqsLifecycleAndReset() {
        String bucketName = "testcontainers-java-bucket";
        String queueName = "testcontainers-java-queue";

        s3.createBucket(b -> b.bucket(bucketName));
        List<String> bucketsBefore = s3.listBuckets().buckets().stream().map(Bucket::name).toList();
        assertTrue(bucketsBefore.contains(bucketName));

        var createRes = sqs.createQueue(q -> q.queueName(queueName));
        String queueUrl = createRes.queueUrl();
        List<String> queuesBefore = sqs.listQueues().queueUrls();
        assertTrue(queuesBefore.contains(queueUrl));

        container.reset();

        List<String> bucketsAfter = s3.listBuckets().buckets().stream().map(Bucket::name).toList();
        assertFalse(bucketsAfter.contains(bucketName));

        List<String> queuesAfter = sqs.listQueues().queueUrls();
        assertFalse(queuesAfter.contains(queueUrl));
    }

    @Test
    public void testResetWithInit() {
        assertDoesNotThrow(() -> container.reset(true));
    }

    @Test
    public void testOptionsValidation() throws IOException {
        Path tempFile = Files.createTempFile("tinystack-test-script", ".sh");
        tempFile.toFile().deleteOnExit();

        TinyStackContainer custom = new TinyStackContainer();
        assertThrows(IllegalArgumentException.class, () -> custom.withInitScripts("invalid-stage", tempFile));
        assertThrows(IllegalArgumentException.class, () -> custom.withInitScripts("ready", Paths.get("/non/existent/script.sh")));

        custom.withInitScripts("ready", tempFile);
        assertThrows(IllegalArgumentException.class, () -> custom.withInitScripts("ready", tempFile));
    }

    @Test
    public void testResetHttpError() throws IOException {
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/_tinystack/reset", exchange -> {
            byte[] response = "reset failed".getBytes();
            exchange.sendResponseHeaders(500, response.length);
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(response);
            }
        });
        server.start();

        int port = server.getAddress().getPort();
        TinyStackContainer mockContainer = new TinyStackContainer() {
            @Override
            public String getEndpointUrl() {
                return "http://127.0.0.1:" + port;
            }
        };

        RuntimeException ex = assertThrows(RuntimeException.class, mockContainer::reset);
        assertTrue(ex.getMessage().contains("500"));
        assertTrue(ex.getMessage().contains("reset failed"));

        server.stop(0);
    }
}
