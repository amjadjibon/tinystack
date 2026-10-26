using DotNet.Testcontainers.Containers;

namespace TinyStack.Testcontainers;

public class TinyStackContainer(TinyStackConfiguration configuration) : DockerContainer(configuration)
{
    public virtual string GetEndpoint() =>
        new UriBuilder("http", Hostname, GetMappedPublicPort(TinyStackBuilder.AwsPort))
        .Uri.GetLeftPart(UriPartial.Authority);

    public string GetEndpointUrl() => GetEndpoint();
    public string GetConnectionString() => GetEndpoint();
    public string GetRegion() => "us-east-1";
    public string GetAccessKey() => "test";
    public string GetAccessKeyId() => GetAccessKey();
    public string GetSecretKey() => "test";
    public string GetSecretAccessKey() => GetSecretKey();

    /// <summary>Clear state; optionally re-run ready scripts. Await outstanding AWS requests first.</summary>
    public async Task ResetAsync(bool init = false, CancellationToken cancellationToken = default)
    {
        using var client = new HttpClient { Timeout = TimeSpan.FromSeconds(60) };
        using var response = await client.PostAsync(GetEndpoint() + "/_tinystack/reset" + (init ? "?init=1" : ""),
            null, cancellationToken).ConfigureAwait(false);
        if (!response.IsSuccessStatusCode)
            throw new HttpRequestException($"reset tinystack: {(int)response.StatusCode}: " +
                await response.Content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false),
                null, response.StatusCode);
    }
}
