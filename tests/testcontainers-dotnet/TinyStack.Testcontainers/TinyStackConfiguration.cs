using Docker.DotNet.Models;
using DotNet.Testcontainers.Configurations;

namespace TinyStack.Testcontainers;

public sealed class TinyStackConfiguration : ContainerConfiguration
{
    public IReadOnlyCollection<string> ScriptDestinations { get; } = Array.Empty<string>();

    public TinyStackConfiguration() { }

    public TinyStackConfiguration(IResourceConfiguration<CreateContainerParameters> configuration)
        : base(configuration) { }

    public TinyStackConfiguration(IContainerConfiguration configuration) : base(configuration) { }

    public TinyStackConfiguration(IReadOnlyCollection<string> destinations) => ScriptDestinations = destinations;

    public TinyStackConfiguration(TinyStackConfiguration oldValue, TinyStackConfiguration newValue)
        : base(oldValue, newValue)
    {
        ScriptDestinations = oldValue.ScriptDestinations.Concat(newValue.ScriptDestinations).Distinct().ToArray();
    }
}
