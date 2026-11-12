package docker

import (
	"reflect"
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
