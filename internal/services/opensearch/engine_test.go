package opensearch

import (
	"slices"
	"testing"
)

func TestEngineSpec(t *testing.T) {
	for _, tc := range []struct {
		version, image string
	}{
		{"OpenSearch_1.0", "opensearchproject/opensearch:1.0.0"},
		{"OpenSearch_1.3", "opensearchproject/opensearch:1.3.0"},
		{"OpenSearch_2.0", "opensearchproject/opensearch:2.0.0"},
		{"OpenSearch_2.11", "opensearchproject/opensearch:2.11.0"},
		{"OpenSearch_2.19", "opensearchproject/opensearch:2.19.0"},
		{"OpenSearch_3.0", "opensearchproject/opensearch:3.0.0"},
		{"OpenSearch_3.9", "opensearchproject/opensearch:3.9.0"},
		{"Elasticsearch_7.10", "docker.elastic.co/elasticsearch/elasticsearch-oss:7.10.2"},
	} {
		spec, err := engineSpec(tc.version)
		if err != nil {
			t.Fatalf("%s: %v", tc.version, err)
		}

		if spec.Image != tc.image {
			t.Errorf("%s: image %s, want %s", tc.version, spec.Image, tc.image)
		}
	}

	for _, version := range []string{
		"OpenSearch_1.4", "OpenSearch_2.20", "OpenSearch_3.10", "OpenSearch_4.0", "OpenSearch_0.9", "OpenSearch_2.011",
		"Elasticsearch_7.9", "Elasticsearch_6.8", "Elasticsearch_7.1", "Elasticsearch_2.3", "Solr_9.0",
	} {
		if _, err := engineSpec(version); err == nil {
			t.Errorf("%s: expected an unsupported engine version", version)
		}
	}
}

func TestEngineSpecContainer(t *testing.T) {
	spec, err := engineSpec("OpenSearch_2.11")
	if err != nil {
		t.Fatal(err)
	}

	for _, env := range []string{
		"discovery.type=single-node", "DISABLE_SECURITY_PLUGIN=true", "DISABLE_INSTALL_DEMO_CONFIG=true",
		"OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m",
	} {
		if !slices.Contains(spec.Env, env) {
			t.Errorf("OpenSearch environment lacks %q: %v", env, spec.Env)
		}
	}

	if spec.DataPath != "/usr/share/opensearch/data" {
		t.Errorf("data path %s", spec.DataPath)
	}

	spec, err = engineSpec("Elasticsearch_7.10")
	if err != nil {
		t.Fatal(err)
	}

	heap := slices.Contains(spec.Env, "ES_JAVA_OPTS=-Xms512m -Xmx512m")
	if !heap || spec.DataPath != "/usr/share/elasticsearch/data" {
		t.Errorf("Elasticsearch container: %v %s", spec.Env, spec.DataPath)
	}
}

func TestResourceSpecIsStable(t *testing.T) {
	arn := domainARN("us-east-1", "000000000000", "logs")
	a, b := (&Service{}).resourceSpec(arn), (&Service{}).resourceSpec(arn)
	if a.Name != b.Name || a.Volume != a.Name+"-data" || a.ARN != arn {
		t.Fatalf("unstable names: %+v %+v", a, b)
	}

	if other := (&Service{}).resourceSpec(domainARN("us-east-1", "000000000000", "logs2")); other.Name == a.Name {
		t.Fatal("distinct domains share a container name")
	}
}

func TestResourceSpecOwnership(t *testing.T) {
	arn := domainARN("us-east-1", "000000000000", "logs")
	a := (&Service{owner: "first"}).resourceSpec(arn)
	b := (&Service{owner: "second"}).resourceSpec(arn)
	if a.Owner != "first" || a.Name == b.Name || a.Volume == b.Volume {
		t.Fatal("stores share domain resources", a, b)
	}
}
