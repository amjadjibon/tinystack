# TinyStack Testcontainers for Java

Java 17+ [Testcontainers](https://java.testcontainers.org/) integration for [TinyStack](https://github.com/amjadjibon/tinystack), the lightweight local AWS emulator.

## Requirements

- Java 17 or later
- Docker-compatible container runtime

## Installation

### Maven

```xml
<dependency>
    <groupId>com.amjadjibon.tinystack</groupId>
    <artifactId>testcontainers-tinystack</artifactId>
    <version>0.1.0</version>
    <scope>test</scope>
</dependency>
```

### Gradle

```groovy
testImplementation 'com.amjadjibon.tinystack:testcontainers-tinystack:0.1.0'
```

## Quick Start

```java
import com.amjadjibon.tinystack.testcontainers.TinyStackContainer;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Test;
import software.amazon.awssdk.auth.credentials.AwsBasicCredentials;
import software.amazon.awssdk.auth.credentials.StaticCredentialsProvider;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.s3.S3Client;

public class MyServiceTest {

    private static TinyStackContainer container;
    private static S3Client s3;

    @BeforeAll
    static void startContainer() {
        container = new TinyStackContainer();
        container.start();

        s3 = S3Client.builder()
                .endpointOverride(container.getEndpointOverride())
                .region(Region.of(container.getRegion()))
                .credentialsProvider(StaticCredentialsProvider.create(
                        AwsBasicCredentials.create(container.getAccessKey(), container.getSecretKey())))
                .forcePathStyle(true)
                .build();
    }

    @AfterAll
    static void stopContainer() {
        if (s3 != null) s3.close();
        if (container != null) container.stop();
    }

    @Test
    void testCreateBucket() {
        s3.createBucket(b -> b.bucket("my-test-bucket"));
        
        // Reset emulator state between tests without restarting container:
        container.reset();
    }
}
```

## Configuration & Init Scripts

```java
TinyStackContainer container = new TinyStackContainer()
    .withConfig(Paths.get("test-config.yaml"))
    .withInitScripts("ready", Paths.get("init-data.sh"))
    .withReadyScriptsComplete();
```

## Resetting State

```java
// Reset all AWS services state
container.reset();

// Reset state and re-execute ready-stage init scripts
container.reset(true);
```

## Running Tests

```bash
./mvnw test
```

The wrapper downloads Maven on first use.
