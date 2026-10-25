package codebuild

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	continueOnFailure = "CONTINUE"
	installPhase      = "install"
	preBuildPhase     = "pre_build"
	postBuildPhase    = "post_build"
)

type buildspec struct {
	Phases    map[string]phaseSpec `yaml:"phases"`
	Artifacts *artifactSpec        `yaml:"artifacts"`
	Version   string               `yaml:"version"`
	Env       struct {
		Variables  map[string]string `yaml:"variables"`
		Parameters map[string]string `yaml:"parameter-store"`
		Secrets    map[string]string `yaml:"secrets-manager"`
		Shell      string            `yaml:"shell"`
	} `yaml:"env"`
}

type phaseSpec struct {
	OnFailure string   `yaml:"on-failure"`
	Commands  []string `yaml:"commands"`
	Finally   []string `yaml:"finally"`
}

type artifactSpec struct {
	BaseDirectory string   `yaml:"base-directory"`
	Name          string   `yaml:"name"`
	Files         []string `yaml:"files"`
	DiscardPaths  bool     `yaml:"discard-paths"`
}

// KnownFields rejects reports, runtime-versions, run-as, cache, batch and all other unimplemented sections.
// https://docs.aws.amazon.com/codebuild/latest/userguide/build-spec-ref.html
func parseBuildspec(data []byte) (*buildspec, error) {
	var spec buildspec
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("invalid buildspec: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("buildspec must contain one YAML document")
	}

	if spec.Version != "0.2" {
		return nil, fmt.Errorf("only buildspec version 0.2 is implemented")
	}

	if spec.Env.Shell != "" && spec.Env.Shell != "bash" && spec.Env.Shell != "/bin/sh" {
		return nil, fmt.Errorf("env.shell supports bash or /bin/sh")
	}

	for name, phase := range spec.Phases {
		switch name {
		case installPhase, preBuildPhase, buildKind, postBuildPhase:
		default:
			return nil, fmt.Errorf("unsupported buildspec phase %q", name)
		}

		if phase.OnFailure != "" && phase.OnFailure != "ABORT" && phase.OnFailure != continueOnFailure {
			return nil, fmt.Errorf("on-failure supports ABORT or CONTINUE")
		}
	}

	seen := map[string]bool{}
	for _, vars := range []map[string]string{spec.Env.Variables, spec.Env.Parameters, spec.Env.Secrets} {
		for name := range vars {
			if seen[name] {
				return nil, fmt.Errorf("duplicate buildspec environment variable %q", name)
			}

			seen[name] = true
			if !envNamePattern.MatchString(name) || strings.HasPrefix(name, "CODEBUILD_") {
				return nil, fmt.Errorf("invalid or reserved buildspec environment name %q", name)
			}
		}
	}

	if spec.Artifacts != nil {
		if len(spec.Artifacts.Files) == 0 {
			return nil, fmt.Errorf("artifacts.files is required")
		}

		for _, pattern := range append(append([]string{}, spec.Artifacts.Files...), spec.Artifacts.BaseDirectory) {
			if strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") {
				return nil, fmt.Errorf("artifact paths must stay in CODEBUILD_SRC_DIR")
			}
		}
	}

	return &spec, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// One shell evaluates every command, retaining working directories and exports across phases (version 0.2).
// A failing install/pre_build aborts build. post_build still follows a failed build.
func (spec *buildspec) script(marker string) string {
	var b strings.Builder
	b.WriteString("cd \"$CODEBUILD_SRC_DIR\" || exit 1\nexport CODEBUILD_BUILD_SUCCEEDING=1\nts_skip=0\nts_abort=0\n")
	for _, name := range []string{installPhase, preBuildPhase, buildKind, postBuildPhase} {
		phase, ok := spec.Phases[name]
		if !ok {
			continue
		}

		conditional := name != installPhase
		if conditional {
			b.WriteString("if [ \"$ts_skip\" = 0 ] && [ \"$ts_abort\" = 0 ]; then\n")
		} else {
			b.WriteString("if [ \"$ts_abort\" = 0 ]; then\n")
		}

		fmt.Fprintf(&b, "printf '\\n%s START %s\\n'\nts_phase=0\n", marker, strings.ToUpper(name))
		b.WriteString("ts_commands() {\n")
		if len(phase.Commands) == 0 {
			b.WriteString(":\n")
		}

		for _, cmd := range phase.Commands {
			fmt.Fprintf(&b, "eval %s\nts_rc=$?\nif [ \"$ts_rc\" != 0 ]; then return \"$ts_rc\"; fi\n", shellQuote(cmd))
		}

		b.WriteString("}\nts_commands\nts_phase=$?\n" +
			"if [ \"$ts_phase\" != 0 ]; then export CODEBUILD_BUILD_SUCCEEDING=0; fi\n")
		// finally executes even after commands fail; its own failure fails the phase.
		for _, cmd := range phase.Finally {
			fmt.Fprintf(&b, "eval %s\nts_rc=$?\nif [ \"$ts_rc\" != 0 ]; then ts_phase=$ts_rc; fi\n", shellQuote(cmd))
		}

		b.WriteString("if [ \"$ts_phase\" != 0 ]; then export CODEBUILD_BUILD_SUCCEEDING=0; ")
		if (name == installPhase || name == preBuildPhase) && phase.OnFailure != continueOnFailure {
			b.WriteString("ts_skip=1; ")
		}

		if phase.OnFailure == "ABORT" {
			b.WriteString("ts_abort=1; ")
		}

		b.WriteString("fi\n")
		fmt.Fprintf(&b, "printf '\\n%s END %s %%s\\n' \"$ts_phase\"\n", marker, strings.ToUpper(name))
		b.WriteString("fi\n")
	}

	b.WriteString("[ \"$CODEBUILD_BUILD_SUCCEEDING\" = 1 ]\n")
	return b.String()
}
