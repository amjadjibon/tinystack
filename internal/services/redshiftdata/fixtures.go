package redshiftdata

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshiftdata"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/mlfixtures"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Target restricts a fixture to a particular provisioned or serverless target.
type Target struct {
	ClusterIdentifier string `json:"clusterIdentifier,omitempty"`
	WorkgroupName     string `json:"workgroupName,omitempty"`
}

// Fixture contains only user-supplied answers. The first matching ID in storage order wins.
type Fixture struct {
	Target                 *Target              `json:"target,omitempty"`
	NumberOfRecordsUpdated *int64               `json:"numberOfRecordsUpdated,omitempty"`
	Error                  *string              `json:"error,omitempty"`
	Database               string               `json:"database,omitempty"`
	Contains               string               `json:"contains,omitempty"`
	Regex                  string               `json:"regex,omitempty"`
	ColumnMetadata         []api.ColumnMetadata `json:"columnMetadata,omitempty"`
	Records                [][]api.Field        `json:"records,omitempty"`
}

func (f *Fixture) validate() error {
	if (f.Contains == "") == (f.Regex == "") {
		return fmt.Errorf("exactly one of contains or regex is required")
	}

	if f.Regex != "" {
		if _, err := regexp.Compile(f.Regex); err != nil {
			return fmt.Errorf("invalid regex: %w", err)
		}
	}

	if f.Target != nil && f.Target.ClusterIdentifier != "" && f.Target.WorkgroupName != "" {
		return fmt.Errorf("target selects one of clusterIdentifier or workgroupName")
	}

	if f.NumberOfRecordsUpdated != nil && (*f.NumberOfRecordsUpdated < 0 || len(f.ColumnMetadata) > 0) {
		return fmt.Errorf("numberOfRecordsUpdated must be nonnegative and cannot accompany a result set")
	}

	if f.Error != nil && (len(f.ColumnMetadata) > 0 || len(f.Records) > 0 || f.NumberOfRecordsUpdated != nil) {
		return fmt.Errorf("error cannot accompany a result")
	}

	for _, row := range f.Records {
		if len(row) != len(f.ColumnMetadata) {
			return fmt.Errorf("record width must match columnMetadata")
		}

		for _, v := range row {
			count := 0
			for _, set := range []bool{
				v.BlobValue != nil, v.BooleanValue != nil, v.DoubleValue != nil,
				v.IsNull != nil, v.LongValue != nil, v.StringValue != nil,
			} {
				if set {
					count++
				}
			}

			if count != 1 || (v.IsNull != nil && !*v.IsNull) {
				return fmt.Errorf("each field must contain exactly one union member; isNull must be true")
			}
		}
	}

	return nil
}

func (f *Fixture) matches(sql, cluster, workgroup, database string) bool {
	if f.Target != nil &&
		((f.Target.ClusterIdentifier != "" && f.Target.ClusterIdentifier != cluster) ||
			(f.Target.WorkgroupName != "" && f.Target.WorkgroupName != workgroup)) {
		return false
	}

	if f.Database != "" && f.Database != database {
		return false
	}

	if f.Contains != "" {
		return strings.Contains(sql, f.Contains)
	}

	re, err := regexp.Compile(f.Regex)
	return err == nil && re.MatchString(sql)
}

func Handler(store storage.Store, account string) http.Handler {
	return mlfixtures.Handler(store, account, "redshift-data", (*Fixture).validate)
}

// substitute replaces bind names outside literals, identifiers and comments. Values are SQL string literals,
// as Data API parameters are strings; no SQL is executed or interpreted.
func substitute(sql string, parameters []api.SqlParameter) (string, error) {
	values := map[string]string{}

	for _, p := range parameters {
		name := common.Value(p.Name)
		if _, ok := values[name]; ok {
			return "", invalid("Duplicate parameter: " + name)
		}

		values[name] = "'" + strings.ReplaceAll(common.Value(p.Value), "'", "''") + "'"
	}

	var out strings.Builder
	for i := 0; i < len(sql); {
		if sql[i] == '\'' || sql[i] == '"' {
			quote := sql[i]
			start := i
			i++
			for i < len(sql) {
				if sql[i] == quote {
					i++
					if i < len(sql) && sql[i] == quote {
						i++
						continue
					}

					break
				}

				i++
			}

			out.WriteString(sql[start:i])
			continue
		}

		if strings.HasPrefix(sql[i:], "--") {
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				end = len(sql) - i
			}

			out.WriteString(sql[i : i+end])
			i += end
			continue
		}

		if strings.HasPrefix(sql[i:], "/*") {
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				out.WriteString(sql[i:])
				break
			}

			end += i + 4
			out.WriteString(sql[i:end])
			i = end
			continue
		}

		if sql[i] == '$' {
			j := i + 1
			for j < len(sql) && parameterCharacter(sql[j]) {
				j++
			}

			if j < len(sql) && sql[j] == '$' {
				delimiter := sql[i : j+1]
				end := strings.Index(sql[j+1:], delimiter)
				if end >= 0 {
					end += j + 1 + len(delimiter)
					out.WriteString(sql[i:end])
					i = end
					continue
				}
			}
		}

		if sql[i] == ':' && i+1 < len(sql) && sql[i+1] == ':' {
			out.WriteString("::")
			i += 2
			continue
		}

		if sql[i] == ':' {
			j := i + 1
			for j < len(sql) && parameterCharacter(sql[j]) {
				j++
			}

			if j > i+1 {
				value, ok := values[sql[i+1:j]]
				if !ok {
					return "", invalid("Missing parameter: " + sql[i+1:j])
				}

				out.WriteString(value)
				i = j
				continue
			}
		}

		out.WriteByte(sql[i])
		i++
	}

	return out.String(), nil
}

func parameterCharacter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}
