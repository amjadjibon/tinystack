package codebuild

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type junitCase struct {
	Failure *struct {
		Message string `xml:"message,attr"`
	} `xml:"failure"`
	Error *struct {
		Message string `xml:"message,attr"`
	} `xml:"error"`
	Skipped *struct{} `xml:"skipped"`
	Name    string    `xml:"name,attr"`
	Class   string    `xml:"classname,attr"`
	Time    string    `xml:"time,attr"`
}

func parseReportFile(data []byte, format string) ([]api.TestCase, error) {
	cases := []api.TestCase{}
	if format == "CUCUMBERJSON" {
		var features []struct {
			Name     string `json:"name"`
			Elements []struct {
				Name  string `json:"name"`
				Type  string `json:"type"`
				Steps []struct {
					Result struct {
						Status   string `json:"status"`
						Error    string `json:"error_message"`
						Duration int64  `json:"duration"`
					} `json:"result"`
				} `json:"steps"`
			} `json:"elements"`
		}

		if err := json.Unmarshal(data, &features); err != nil {
			return nil, err
		}

		for _, f := range features {
			for _, e := range f.Elements {
				if e.Type == "background" {
					continue
				}

				c := api.TestCase{
					Name:                  new(e.Name),
					TestSuiteName:         new(f.Name),
					Status:                new(statusSucceeded),
					DurationInNanoSeconds: new(int64(0)),
				}

				for _, step := range e.Steps {
					if step.Result.Duration < 0 || step.Result.Duration > math.MaxInt64-*c.DurationInNanoSeconds {
						return nil, fmt.Errorf("invalid Cucumber duration")
					}

					*c.DurationInNanoSeconds += step.Result.Duration
					switch step.Result.Status {
					case "passed":
					case "failed":
						c.Status = new(statusFailed)
						c.Message = new(step.Result.Error)
					case "skipped", "pending", "undefined":
						if *c.Status == statusSucceeded {
							c.Status = new("SKIPPED")
						}

					default:
						return nil, fmt.Errorf("unsupported Cucumber step status")
					}
				}

				cases = append(cases, c)
			}
		}

		return cases, nil
	}

	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	suites := []string{}
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, err
		}

		if end, ok := token.(xml.EndElement); ok && end.Name.Local == "testsuite" && len(suites) > 0 {
			suites = suites[:len(suites)-1]
		}

		start, ok := token.(xml.StartElement)
		if ok && start.Name.Local == "testsuite" {
			name := ""
			for _, attr := range start.Attr {
				if attr.Name.Local == "name" {
					name = attr.Value
				}
			}

			suites = append(suites, name)
		}

		if !ok || start.Name.Local != "testcase" {
			continue
		}

		var raw junitCase
		if err := decoder.DecodeElement(&raw, &start); err != nil {
			return nil, err
		}

		seconds := float64(0)
		if raw.Time != "" {
			var err error
			seconds, err = strconv.ParseFloat(raw.Time, 64)
			if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) ||
				seconds > float64(math.MaxInt64)/1e9 {
				return nil, fmt.Errorf("invalid JUnit duration")
			}
		}

		c := api.TestCase{
			Name:                  new(raw.Name),
			Prefix:                new(raw.Class),
			TestSuiteName:         new(raw.Class),
			Status:                new(statusSucceeded),
			DurationInNanoSeconds: new(int64(seconds * 1e9)),
		}

		if len(suites) > 0 && suites[len(suites)-1] != "" {
			c.TestSuiteName = new(suites[len(suites)-1])
		}

		if raw.Skipped != nil {
			c.Status = new("SKIPPED")
		}

		if raw.Failure != nil {
			c.Status = new(statusFailed)
			c.Message = new(raw.Failure.Message)
		}

		if raw.Error != nil {
			c.Status = new("ERROR")
			c.Message = new(raw.Error.Message)
		}

		cases = append(cases, c)
	}

	return cases, nil
}

func (s *Service) collectReports(ctx context.Context, key string, r *buildRecord, spec *buildspec, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}

	defer func() { _ = root.Close() }()
	for group, decl := range spec.Reports {
		cases := []api.TestCase{}
		err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			if entry.IsDir() {
				return nil
			}

			relative := name
			if decl.BaseDirectory != "" && decl.BaseDirectory != "." {
				var ok bool
				relative, ok = strings.CutPrefix(name, strings.TrimSuffix(decl.BaseDirectory, "/")+"/")
				if !ok {
					return nil
				}
			}

			selected := false
			for _, pattern := range decl.Files {
				if matchArtifact(pattern, relative) {
					selected = true
					break
				}
			}

			if !selected {
				return nil
			}

			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("report symlinks are unsupported")
			}

			data, e := root.ReadFile(name)
			if e != nil {
				return e
			}

			parsed, e := parseReportFile(data, decl.FileFormat)
			if e != nil {
				return e
			}

			rawPath := name
			if decl.DiscardPaths {
				rawPath = path.Base(name)
			}

			for i := range parsed {
				parsed[i].TestRawDataPath = new(rawPath)
			}

			cases = append(cases, parsed...)
			return nil
		})
		if err != nil {
			return err
		}

		if len(cases) == 0 {
			continue
		}

		groupARN := group
		if !strings.HasPrefix(group, "arn:") {
			groupARN = reportGroupARN(r.Scope, common.Value(r.Build.ProjectName)+"-"+group)
		}

		hex := common.RandomID(32, "0123456789abcdef")
		uuid := hex[:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:]
		id := strings.Replace(groupARN, ":report-group/", ":report/", 1) + ":" + uuid
		summary := &api.TestReportSummary{
			Total:                 new(int64(len(cases))),
			StatusCounts:          map[string]int64{},
			DurationInNanoSeconds: new(int64(0)),
		}

		for i := range cases {
			cases[i].ReportArn = new(id)
			cases[i].Expired = new(stamp() + 30*86400)
			summary.StatusCounts[common.Value(cases[i].Status)]++
			*summary.DurationInNanoSeconds += common.Value(cases[i].DurationInNanoSeconds)
		}

		status := statusSucceeded
		if summary.StatusCounts[statusFailed]+summary.StatusCounts["ERROR"] > 0 {
			status = statusFailed
		}

		err = s.store.Update(ctx, func(tx storage.WriteTx) error {
			build, found, e := storage.Read[buildRecord](tx, key)
			if e != nil {
				return e
			}

			if !found || common.Value(build.Build.BuildComplete) {
				return context.Canceled
			}

			g, found, e := storage.Read[api.ReportGroup](tx, r.Scope.Key(serviceName, reportGroupKind, groupARN))
			if e != nil {
				return e
			}

			if !found {
				if strings.HasPrefix(group, "arn:") {
					return reportMissing()
				}

				g = &api.ReportGroup{
					Arn:          new(groupARN),
					Name:         new(common.Value(r.Build.ProjectName) + "-" + group),
					Type:         new("TEST"),
					ExportConfig: &api.ReportExportConfig{ExportConfigType: new("NO_EXPORT")},
					Created:      new(stamp()),
					LastModified: new(stamp()),
					Status:       new("ACTIVE"),
				}

				if e := validateReportGroup(g); e != nil {
					return e
				}

				if e := storage.Write(tx, r.Scope.Key(serviceName, reportGroupKind, groupARN), g); e != nil {
					return e
				}
			}

			truncated := len(cases) > 500
			if truncated {
				slices.SortStableFunc(cases, func(a, b api.TestCase) int {
					if common.Value(a.Status) == statusFailed && common.Value(b.Status) != statusFailed {
						return -1
					}

					if common.Value(b.Status) == statusFailed && common.Value(a.Status) != statusFailed {
						return 1
					}

					return 0
				})
				cases = cases[:500]
			}

			report := api.Report{
				Arn:            new(id),
				Name:           r.Build.Id,
				Type:           new("TEST"),
				ReportGroupArn: new(groupARN),
				ExecutionId:    r.Build.Arn,
				Status:         new(status),
				Created:        new(stamp()),
				Expired:        new(stamp() + 30*86400),
				ExportConfig:   g.ExportConfig,
				Truncated:      new(truncated),
				TestSummary:    summary,
			}

			if e := storage.Write(
				tx,
				r.Scope.Key(serviceName, reportKind, id),
				reportRecord{Report: report, Cases: cases},
			); e != nil {
				return e
			}

			build.Build.ReportArns = append(build.Build.ReportArns, id)
			return storage.Write(tx, key, build)
		})
		if err != nil {
			return err
		}
	}

	return nil
}
