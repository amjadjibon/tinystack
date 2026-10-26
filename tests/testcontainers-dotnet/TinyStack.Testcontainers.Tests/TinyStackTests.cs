using System.Diagnostics;
using System.Net;
using System.Net.Sockets;
using System.Text.Json;
using Amazon.Runtime;
using Amazon.S3;
using Amazon.S3.Model;
using Amazon.SQS;
using Amazon.SQS.Model;
using Amazon.SecurityToken;
using Amazon.SecurityToken.Model;
using Xunit;

namespace TinyStack.Testcontainers.Tests;

public class TinyStackTests
{
    [Fact]
    public async Task AwsLifecycleConfigInitAndReset()
    {
        var directory = Path.Combine(Path.GetTempPath(), "tinystack-dotnet-" + Guid.NewGuid().ToString("N"));
        Directory.CreateDirectory(directory);
        string? builtImage = null;
        try
        {
            var image = Environment.GetEnvironmentVariable("TINYSTACK_IMAGE");
            if (string.IsNullOrWhiteSpace(image))
            {
                builtImage = "tinystack-test-dotnet:" + Guid.NewGuid().ToString("N");
                image = builtImage;
                await Docker("build", "--target", "headless", "-t", image, FindRoot());
            }

            var config = Path.Combine(directory, "custom.yaml");
            var script = Path.Combine(directory, "10-ready.sh");
            await File.WriteAllTextAsync(config, "defaults:\n  region: us-west-2\n");
            await File.WriteAllTextAsync(script,
                "#!/bin/sh\nset -eu\nsleep 1\nprintf '%s' \"$AWS_DEFAULT_REGION\" >> /tmp/ready-marker\n");
            await using var container = new TinyStackBuilder(image).WithConfig(config)
                .WithInitScripts("ready", script).WithReadyScriptsComplete().Build();
            await container.StartAsync();
            Assert.Equal(container.GetEndpoint(), container.GetConnectionString());
            Assert.Equal(container.GetEndpoint(), container.GetEndpointUrl());
            Assert.Equal("us-east-1", container.GetRegion());
            Assert.Equal("test", container.GetAccessKeyId());
            Assert.Equal("test", container.GetSecretAccessKey());

            using var http = new HttpClient();
            using var health = JsonDocument.Parse(
                await http.GetStringAsync(container.GetEndpoint() + "/_tinystack/health"));
            Assert.Equal("ok", health.RootElement.GetProperty("status").GetString());
            Assert.True(health.RootElement.TryGetProperty("services", out _));
            Assert.True(health.RootElement.TryGetProperty("storage", out _));
            await AssertReady(http, container);
            Assert.Equal("us-west-2", (await container.ExecAsync(new[] { "cat", "/tmp/ready-marker" })).Stdout);

            var credentials = new BasicAWSCredentials(container.GetAccessKey(), container.GetSecretKey());
            using var s3 = new AmazonS3Client(credentials, new AmazonS3Config
            {
                ServiceURL = container.GetEndpoint(),
                AuthenticationRegion = container.GetRegion(),
                ForcePathStyle = true
            });
            using var sqs = new AmazonSQSClient(credentials, new AmazonSQSConfig
            {
                ServiceURL = container.GetEndpoint(),
                AuthenticationRegion = container.GetRegion()
            });
            using var sts = new AmazonSecurityTokenServiceClient(credentials, new AmazonSecurityTokenServiceConfig
            {
                ServiceURL = container.GetEndpoint(),
                AuthenticationRegion = container.GetRegion()
            });
            var identity = await sts.GetCallerIdentityAsync(new GetCallerIdentityRequest());
            Assert.Equal("000000000000", identity.Account);
            Assert.Contains(identity.Account, identity.Arn);
            await s3.PutBucketAsync(new PutBucketRequest { BucketName = "dotnet-bucket" });
            Assert.Contains((await s3.ListBucketsAsync()).Buckets, bucket => bucket.BucketName == "dotnet-bucket");
            await s3.PutObjectAsync(new PutObjectRequest
            {
                BucketName = "dotnet-bucket",
                Key = "hello.txt",
                ContentBody = "hello TinyStack"
            });
            using (var result = await s3.GetObjectAsync("dotnet-bucket", "hello.txt"))
            using (var reader = new StreamReader(result.ResponseStream))
                Assert.Equal("hello TinyStack", await reader.ReadToEndAsync());
            var queue = await sqs.CreateQueueAsync(new CreateQueueRequest { QueueName = "dotnet-queue" });
            Assert.Contains(queue.QueueUrl, (await sqs.ListQueuesAsync(new ListQueuesRequest())).QueueUrls);
            await sqs.SendMessageAsync(queue.QueueUrl, "hello queue");
            var messages = await sqs.ReceiveMessageAsync(new ReceiveMessageRequest { QueueUrl = queue.QueueUrl });
            var message = Assert.Single(messages.Messages);
            Assert.Equal("hello queue", message.Body);
            await sqs.DeleteMessageAsync(queue.QueueUrl, message.ReceiptHandle);

            await container.ResetAsync();
            Assert.Empty((await s3.ListBucketsAsync()).Buckets ?? []);
            Assert.Empty((await sqs.ListQueuesAsync(new ListQueuesRequest())).QueueUrls ?? []);
            Assert.Equal("us-west-2", (await container.ExecAsync(new[] { "cat", "/tmp/ready-marker" })).Stdout);
            await container.ResetAsync(init: true);
            await AssertReady(http, container);
            Assert.Equal("us-west-2us-west-2",
                (await container.ExecAsync(new[] { "cat", "/tmp/ready-marker" })).Stdout);
        }
        finally
        {
            try
            {
                if (builtImage != null) await Docker("image", "rm", builtImage);
            }
            finally { Directory.Delete(directory, true); }
        }
    }

    [Fact]
    public void OptionsAreValidatedAndBuildersAreImmutable()
    {
        var file = Path.GetTempFileName();
        try
        {
            var builder = new TinyStackBuilder("tinystack:test");
            Assert.Throws<ArgumentException>(() => builder.WithInitScripts("invalid", file));
            Assert.Throws<ArgumentException>(() => builder.WithInitScripts("ready", file + ".missing"));
            Assert.Throws<ArgumentException>(() => builder.WithConfig(Path.GetTempPath()));
            var configured = builder.WithInitScripts("ready", file).WithEnvironment("CHECK", "1");
            Assert.Throws<ArgumentException>(() => configured.WithInitScripts("ready", file));
            builder.WithInitScripts("ready", file); // Configuring a derived builder did not mutate the original.
        }
        finally { File.Delete(file); }
    }

    [Fact]
    public async Task ResetReportsHttpErrorsAndHonorsCancellation()
    {
        var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        try
        {
            var endpoint = $"http://127.0.0.1:{((IPEndPoint)listener.LocalEndpoint).Port}";
            var server = Task.Run(async () =>
            {
                using var connection = await listener.AcceptTcpClientAsync();
                await using var stream = connection.GetStream();
                using var reader = new StreamReader(stream, leaveOpen: true);
                Assert.Equal("POST /_tinystack/reset?init=1 HTTP/1.1", await reader.ReadLineAsync());
                while (!string.IsNullOrEmpty(await reader.ReadLineAsync())) { }
                await stream.WriteAsync(System.Text.Encoding.ASCII.GetBytes(
                    "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 12\r\n" +
                    "Connection: close\r\n\r\nreset failed"));
            });
            await using var container = new EndpointContainer(endpoint);
            var error = await Assert.ThrowsAsync<HttpRequestException>(() => container.ResetAsync(true));
            Assert.Equal(HttpStatusCode.InternalServerError, error.StatusCode);
            Assert.Contains("reset failed", error.Message);
            await server;
            using var cancellation = new CancellationTokenSource();
            cancellation.Cancel();
            await Assert.ThrowsAnyAsync<OperationCanceledException>(() =>
                container.ResetAsync(cancellationToken: cancellation.Token));
        }
        finally { listener.Stop(); }
    }

    private sealed class EndpointContainer(string endpoint)
        : TinyStackContainer(new TestConfigurationBuilder().Configuration)
    {
        public override string GetEndpoint() => endpoint;
    }

    private sealed class TestConfigurationBuilder() : DotNet.Testcontainers.Builders.ContainerBuilder("tinystack:test")
    {
        public TinyStackConfiguration Configuration => new(DockerResourceConfiguration);
    }

    private static async Task AssertReady(HttpClient http, TinyStackContainer container)
    {
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(120));
        while (true)
        {
            using var response = await http.GetAsync(container.GetEndpoint() + "/_tinystack/init/ready", timeout.Token);
            response.EnsureSuccessStatusCode();
            using var status = JsonDocument.Parse(await response.Content.ReadAsStringAsync(timeout.Token));
            if (status.RootElement.GetProperty("completed").GetBoolean())
            {
                Assert.Equal("SUCCESSFUL", status.RootElement.GetProperty("state").GetString());
                Assert.Equal(0, status.RootElement.GetProperty("scripts")[0].GetProperty("exit_code").GetInt32());
                return;
            }
            await Task.Delay(100, timeout.Token);
        }
    }

    private static string FindRoot()
    {
        var directory = new DirectoryInfo(AppContext.BaseDirectory);
        while (directory != null && !File.Exists(Path.Combine(directory.FullName, "Dockerfile")))
            directory = directory.Parent;
        return directory?.FullName ?? throw new InvalidOperationException("Repository Dockerfile not found");
    }

    private static async Task Docker(params string[] arguments)
    {
        var start = new ProcessStartInfo("docker") { UseShellExecute = false };
        foreach (var argument in arguments) start.ArgumentList.Add(argument);
        using var process = Process.Start(start) ?? throw new InvalidOperationException("Cannot start docker");
        await process.WaitForExitAsync();
        Assert.True(process.ExitCode == 0, $"docker {string.Join(' ', arguments)} exited {process.ExitCode}");
    }
}
