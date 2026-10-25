package opensearch

import (
	"strconv"
	"strings"
)

const (
	// heapOptions is TinyStack's choice, not an AWS setting: a conservative heap so a few domains fit on a laptop.
	heapOptions = "-Xms512m -Xmx512m"

	openSearchImage = "opensearchproject/opensearch"
	openSearchData  = "/usr/share/opensearch/data"

	// Elasticsearch_7.10 is the last Elasticsearch version AWS offers, built on Elastic's 7.10.2 OSS distribution.
	elasticsearchVersion = "Elasticsearch_7.10"
	elasticsearchImage   = "docker.elastic.co/elasticsearch/elasticsearch-oss:7.10.2"
	elasticsearchData    = "/usr/share/elasticsearch/data"
)

// lastMinor is the highest minor version of each OpenSearch major that has an opensearchproject/opensearch
// X.Y.0 tag on Docker Hub. Every lower minor has one too.
var lastMinor = map[string]int{"1": 3, "2": 19, "3": 9}

// engineSpec resolves an EngineVersion to the image, data path and environment of a single-node container.
func engineSpec(version string) (*Spec, error) {
	if version == elasticsearchVersion {
		return &Spec{
			Image:    elasticsearchImage,
			DataPath: elasticsearchData,
			Env:      []string{"discovery.type=single-node", "ES_JAVA_OPTS=" + heapOptions},
		}, nil
	}

	numbers, ok := strings.CutPrefix(version, "OpenSearch_")
	major, minor, _ := strings.Cut(numbers, ".")
	last, known := lastMinor[major]
	n, err := strconv.Atoi(minor)
	if !ok || !known || err != nil || strconv.Itoa(n) != minor || n > last {
		return nil, unsupported(
			"EngineVersion %s has no engine image in docker mode; use %s or OpenSearch_X.Y up to 1.%d, 2.%d or 3.%d",
			version, elasticsearchVersion, lastMinor["1"], lastMinor["2"], lastMinor["3"],
		)
	}

	return &Spec{
		Image:    openSearchImage + ":" + major + "." + minor + ".0",
		DataPath: openSearchData,
		Env: []string{
			"discovery.type=single-node",
			// Plain HTTP on 9200: TinyStack does not authenticate the data plane.
			"DISABLE_SECURITY_PLUGIN=true",
			"DISABLE_INSTALL_DEMO_CONFIG=true",
			"OPENSEARCH_JAVA_OPTS=" + heapOptions,
		},
	}, nil
}
