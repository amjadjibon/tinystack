package dockercli

import (
	"context"
	"slices"
	"testing"
)

func TestCommandUsesDockerAlias(t *testing.T) {
	for _, tc := range []struct {
		alias string
		want  []string
	}{
		{"", []string{"docker", "ps", "-a"}},
		{"podman", []string{"podman", "ps", "-a"}},
		{"  nerdctl --namespace k8s.io ", []string{"nerdctl", "--namespace", "k8s.io", "ps", "-a"}},
	} {
		t.Setenv("DOCKER_ALIAS", tc.alias)
		if got := Command(context.Background(), "ps", "-a").Args; !slices.Equal(got, tc.want) {
			t.Errorf("DOCKER_ALIAS=%q: got %q, want %q", tc.alias, got, tc.want)
		}
	}
}
