using System.Text.Json;
using Docker.DotNet.Models;
using DotNet.Testcontainers.Builders;
using DotNet.Testcontainers.Configurations;

namespace TinyStack.Testcontainers;

public sealed class TinyStackBuilder : ContainerBuilder<TinyStackBuilder, TinyStackContainer, TinyStackConfiguration>
{
    public const ushort AwsPort = 4566;
    public const string DefaultImage = "ghcr.io/amjadjibon/tinystack:latest";

    public TinyStackBuilder() : this(new TinyStackConfiguration())
    {
        var image = Environment.GetEnvironmentVariable("TINYSTACK_IMAGE");
        DockerResourceConfiguration = Init().WithImage(string.IsNullOrWhiteSpace(image) ? DefaultImage : image.Trim())
            .DockerResourceConfiguration;
    }

    public TinyStackBuilder(string image) : this() =>
        DockerResourceConfiguration = WithImage(image).DockerResourceConfiguration;

    private TinyStackBuilder(TinyStackConfiguration configuration) : base(configuration) =>
        DockerResourceConfiguration = configuration;

    protected override TinyStackConfiguration DockerResourceConfiguration { get; }

    public override TinyStackContainer Build()
    {
        Validate();
        return new TinyStackContainer(DockerResourceConfiguration);
    }

    protected override TinyStackBuilder Init() => base.Init()
        .WithPortBinding(AwsPort, true)
        .WithCommand("start", "--host", "0.0.0.0", "--port", "4566")
        .WithWaitStrategy(Wait.ForUnixContainer().UntilHttpRequestIsSucceeded(
            request => request.ForPort(AwsPort).ForPath("/_tinystack/health"),
            strategy => strategy.WithTimeout(TimeSpan.FromSeconds(120))));

    public TinyStackBuilder WithConfig(string path)
    {
        RequireFile(path);
        return WithResourceMapping(new FileInfo(path), new FileInfo("/etc/tinystack/config.yaml"))
            .WithCommand("--config", "/etc/tinystack/config.yaml");
    }

    public TinyStackBuilder WithInitScripts(string stage, params string[] files)
    {
        if (stage is not ("boot" or "start" or "ready" or "shutdown"))
            throw new ArgumentException($"unsupported init stage: {stage}", nameof(stage));

        var builder = this;
        foreach (var file in files)
        {
            RequireFile(file);
            var destination = $"/etc/tinystack/init/{stage}.d/{Path.GetFileName(file)}";
            if (builder.DockerResourceConfiguration.ScriptDestinations.Contains(destination))
                throw new ArgumentException($"duplicate init script destination: {destination}", nameof(files));

            var mode = OperatingSystem.IsWindows() ? Unix.FileMode644 : (UnixFileModes)File.GetUnixFileMode(file);
            builder = builder.WithResourceMapping(new FileInfo(file), new FileInfo(destination), fileMode: mode);
            builder = builder.Merge(builder.DockerResourceConfiguration,
                new TinyStackConfiguration(new[] { destination }));
        }
        return builder;
    }

    public TinyStackBuilder WithReadyScriptsComplete() => WithWaitStrategy(
        Wait.ForUnixContainer()
            .UntilHttpRequestIsSucceeded(request => request.ForPort(AwsPort).ForPath("/_tinystack/health"),
                strategy => strategy.WithTimeout(TimeSpan.FromSeconds(120)))
            .UntilHttpRequestIsSucceeded(request => request.ForPort(AwsPort).ForPath("/_tinystack/init/ready")
                .ForResponseMessageMatching(async response =>
                {
                    using var body = JsonDocument.Parse(await response.Content.ReadAsStringAsync());
                    return body.RootElement.GetProperty("completed").GetBoolean()
                        && body.RootElement.GetProperty("state").GetString() == "SUCCESSFUL";
                }), strategy => strategy.WithTimeout(TimeSpan.FromSeconds(120))));

    private static void RequireFile(string path)
    {
        if (!File.Exists(path))
            throw new ArgumentException($"file {path} is not a regular file", nameof(path));
    }

    protected override TinyStackBuilder Clone(IResourceConfiguration<CreateContainerParameters> configuration) =>
        Merge(DockerResourceConfiguration, new TinyStackConfiguration(configuration));

    protected override TinyStackBuilder Clone(IContainerConfiguration configuration) =>
        Merge(DockerResourceConfiguration, new TinyStackConfiguration(configuration));

    protected override TinyStackBuilder Merge(TinyStackConfiguration oldValue, TinyStackConfiguration newValue) =>
        new(new TinyStackConfiguration(oldValue, newValue));
}
