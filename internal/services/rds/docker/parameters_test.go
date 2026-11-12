package docker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/amjadjibon/tinystack/internal/services/rds"
)

func TestParameterArguments(t *testing.T) {
	for _, test := range []struct {
		engine string
		want   []string
	}{
		{rds.EnginePostgres, []string{"postgres", "-c", "max_connections=200", "-c", "work_mem=8192"}},
		{rds.EngineMySQL, []string{"mysqld", "--max-connections=200", "--work-mem=8192"}},
		{rds.EngineMariaDB, []string{"mariadbd", "--max-connections=200", "--work-mem=8192"}},
	} {
		got := parameterArguments(
			&rds.Spec{Engine: test.engine, Parameters: map[string]string{"work_mem": "8192", "max_connections": "200"}},
		)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: %v", test.engine, got)
		}
	}

	if got := parameterArguments(&rds.Spec{}); len(got) != 0 {
		t.Fatalf("empty parameters: %v", got)
	}
}

func TestDocumentDBAndNeptuneParameters(t *testing.T) {
	got := parameterArguments(&rds.Spec{Engine: rds.EngineDocDB, Parameters: map[string]string{
		"profiler": "enabled", "profiler_threshold_ms": "250", "profiler_sampling_rate": "0.5",
		"ttl_monitor": "disabled", "tls": "disabled", "audit_logs": "disabled",
	}})
	want := []string{
		"mongod", "--profile", "1", "--slowms", "250", "--slowOpSampleRate", "0.5",
		"--setParameter", "ttlMonitorEnabled=false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("documentdb: %v", got)
	}

	script := gremlinStartup(&rds.Spec{Parameters: map[string]string{"neptune_query_timeout": "5000"}})
	if !strings.Contains(script, "echo 'evaluationTimeout: 5000' >> conf/gremlin-server.yaml") ||
		strings.Contains(gremlinStartup(&rds.Spec{}), "evaluationTimeout") {
		t.Fatal(script)
	}
}
