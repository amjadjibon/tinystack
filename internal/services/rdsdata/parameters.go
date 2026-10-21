package rdsdata

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rdsdata"
	"github.com/amjadjibon/tinystack/internal/common"
)

func parameterValue(p api.SqlParameter) (any, error) {
	f := p.Value
	if f == nil {
		return nil, awserror.New(badRequest, "Parameter value is required")
	}

	count := 0
	var value any
	if f.IsNull != nil {
		count++
		if !*f.IsNull {
			return nil, awserror.New(badRequest, "isNull must be true")
		}
	}

	if f.StringValue != nil {
		count++
		value = *f.StringValue
	}

	if f.LongValue != nil {
		count++
		value = *f.LongValue
	}

	if f.DoubleValue != nil {
		count++
		value = *f.DoubleValue
	}

	if f.BooleanValue != nil {
		count++
		value = *f.BooleanValue
	}

	if f.BlobValue != nil {
		count++
		value = *f.BlobValue
	}

	if count != 1 {
		return nil, awserror.New(badRequest, "Exactly one Field member is required")
	}

	if p.TypeHint != nil {
		switch *p.TypeHint {
		case hintDate, hintTime, hintTimestamp, hintDecimal, "UUID", hintJSON:
		default:
			return nil, awserror.New(badRequest, "Unsupported typeHint")
		}

		if f.StringValue == nil && f.IsNull == nil {
			return nil, awserror.New(badRequest, "typeHint requires a string or null")
		}
	}

	return value, nil
}

// bind replaces named parameters only in SQL code, preserving quotes, comments and PostgreSQL casts.
func bind(query, driver string, parameters []api.SqlParameter) (statement string, arguments []any, bindErr error) {
	values := map[string]api.SqlParameter{}
	for _, p := range parameters {
		name := common.Value(p.Name)
		if name == "" {
			return "", nil, awserror.New(badRequest, "Parameter name is required")
		}

		if _, found := values[name]; found {
			return "", nil, awserror.New(badRequest, "Duplicate parameter: "+name)
		}

		if _, err := parameterValue(p); err != nil {
			return "", nil, err
		}

		values[name] = p
	}

	used := map[string]bool{}
	var out strings.Builder
	var args []any
	ended := false
	for i := 0; i < len(query); {
		c := query[i]
		if ended && !unicode.IsSpace(rune(c)) && c != ';' &&
			!strings.HasPrefix(query[i:], "--") && !strings.HasPrefix(query[i:], "/*") &&
			(driver != driverMySQL || c != '#') {
			return "", nil, awserror.New(badRequest, "Multiple SQL statements are unsupported")
		}

		if c == '\'' || c == '"' || c == '`' {
			start, quote := i, c
			i++
			for i < len(query) {
				if query[i] == '\\' && i+1 < len(query) {
					i += 2
					continue
				}

				if query[i] == quote {
					i++
					if i < len(query) && query[i] == quote {
						i++
						continue
					}

					break
				}

				i++
			}

			out.WriteString(query[start:i])
			continue
		}

		if strings.HasPrefix(query[i:], "--") || (driver == driverMySQL && c == '#') {
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				out.WriteString(query[i:])
				break
			}

			out.WriteString(query[i : i+end+1])
			i += end + 1
			continue
		}

		if strings.HasPrefix(query[i:], "/*") {
			end, depth := i+2, 1
			for end < len(query) && depth > 0 {
				switch {
				case strings.HasPrefix(query[end:], "/*"):
					depth++
					end += 2
				case strings.HasPrefix(query[end:], "*/"):
					depth--
					end += 2
				default:
					end++
				}
			}

			out.WriteString(query[i:end])
			i = end
			continue
		}

		if c == '$' && driver == driverPostgres {
			j := i + 1
			for j < len(query) && (unicode.IsLetter(rune(query[j])) || query[j] == '_' || unicode.IsDigit(rune(query[j]))) {
				j++
			}

			if j < len(query) && query[j] == '$' {
				delimiter := query[i : j+1]
				if end := strings.Index(query[j+1:], delimiter); end >= 0 {
					end += j + 1 + len(delimiter)
					out.WriteString(query[i:end])
					i = end
					continue
				}
			}
		}

		if strings.HasPrefix(query[i:], "::") {
			out.WriteString("::")
			i += 2
			continue
		}

		if c == ':' && i+1 < len(query) && (unicode.IsLetter(rune(query[i+1])) || query[i+1] == '_') {
			j := i + 2
			for j < len(query) && (unicode.IsLetter(rune(query[j])) || unicode.IsDigit(rune(query[j])) || query[j] == '_') {
				j++
			}

			name := query[i+1 : j]
			p, found := values[name]
			if !found {
				return "", nil, awserror.New(badRequest, "Missing parameter: "+name)
			}

			value, err := parameterValue(p)
			if err != nil {
				return "", nil, err
			}

			args = append(args, value)
			used[name] = true
			placeholder := "?"
			if driver == driverPostgres {
				placeholder = fmt.Sprintf("$%d", len(args))
				if p.TypeHint == nil {
					sqlType := ""
					switch {
					case p.Value.LongValue != nil:
						sqlType = typeBigInt
					case p.Value.DoubleValue != nil:
						sqlType = "DOUBLE PRECISION"
					case p.Value.BooleanValue != nil:
						sqlType = typeBoolean
					case p.Value.BlobValue != nil:
						sqlType = typeBytea
					}

					if sqlType != "" {
						placeholder = "CAST(" + placeholder + " AS " + sqlType + ")"
					}
				}
			}

			if p.TypeHint != nil {
				hint := *p.TypeHint
				if driver == driverPostgres {
					placeholder = "CAST(" + placeholder + " AS " + hint + ")"
				} else {
					switch hint {
					case hintDate, hintTime, hintJSON:
						placeholder = "CAST(" + placeholder + " AS " + hint + ")"
					case hintTimestamp:
						placeholder = "CAST(" + placeholder + " AS DATETIME)"
					case hintDecimal:
						placeholder = "CAST(" + placeholder + " AS DECIMAL(65,30))"
					}
				}
			}

			out.WriteString(placeholder)
			i = j
			continue
		}

		if c == ';' {
			ended = true
		}

		out.WriteByte(c)
		i++
	}

	for name := range values {
		if !used[name] {
			return "", nil, awserror.New(badRequest, "Unused parameter: "+name)
		}
	}

	return out.String(), args, nil
}

// sqlWords ignores literal values and comments when identifying statements and RETURNING clauses.
func sqlWords(query string) []string {
	var words []string
	for i := 0; i < len(query); {
		c := query[i]
		if c == '\'' || c == '"' || c == '`' {
			quote := c
			i++
			for i < len(query) {
				if query[i] == '\\' && i+1 < len(query) {
					i += 2
					continue
				}

				if query[i] == quote {
					i++
					if i < len(query) && query[i] == quote {
						i++
						continue
					}

					break
				}

				i++
			}

			continue
		}

		if strings.HasPrefix(query[i:], "--") || c == '#' {
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				break
			}

			i += end + 1
			continue
		}

		if strings.HasPrefix(query[i:], "/*") {
			i += 2
			depth := 1
			for i < len(query) && depth > 0 {
				switch {
				case strings.HasPrefix(query[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(query[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}

			continue
		}

		if c == '$' {
			j := i + 1
			for j < len(query) && (unicode.IsLetter(rune(query[j])) || unicode.IsDigit(rune(query[j])) || query[j] == '_') {
				j++
			}

			if j < len(query) && query[j] == '$' {
				delimiter := query[i : j+1]
				if end := strings.Index(query[j+1:], delimiter); end >= 0 {
					i = j + 1 + end + len(delimiter)
					continue
				}
			}
		}

		if unicode.IsLetter(rune(c)) {
			j := i + 1
			for j < len(query) && (unicode.IsLetter(rune(query[j])) || unicode.IsDigit(rune(query[j])) || query[j] == '_') {
				j++
			}

			words = append(words, strings.ToUpper(query[i:j]))
			i = j
			continue
		}

		i++
	}

	return words
}
