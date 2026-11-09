package xray

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"text/scanner"
)

type traceFilter func(traceRecord) bool

type filterParser struct {
	tokens []string
	pos    int
}

var annotationKey = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func parseFilter(expression string) (traceFilter, error) {
	if strings.TrimSpace(expression) == "" {
		return func(traceRecord) bool { return true }, nil
	}

	if len(expression) > 2048 {
		return nil, invalid("unsupported filter: exceeds 2048 bytes")
	}

	var lex scanner.Scanner
	lex.Init(strings.NewReader(expression))
	lex.Mode = scanner.ScanIdents | scanner.ScanFloats | scanner.ScanStrings
	var scanErr error
	lex.Error = func(_ *scanner.Scanner, msg string) { scanErr = invalid("unsupported filter: %s", msg) }
	p := &filterParser{}
	for tok := lex.Scan(); tok != scanner.EOF; tok = lex.Scan() {
		p.tokens = append(p.tokens, lex.TokenText())
	}

	if scanErr != nil {
		return nil, scanErr
	}

	f, err := p.or()
	if err != nil {
		return nil, err
	}

	if p.pos != len(p.tokens) {
		return nil, invalid("unsupported filter part %q", p.tokens[p.pos])
	}

	return f, nil
}

func (p *filterParser) take(s string) bool {
	if p.pos < len(p.tokens) && strings.EqualFold(p.tokens[p.pos], s) {
		p.pos++
		return true
	}

	return false
}

func (p *filterParser) next() string {
	if p.pos == len(p.tokens) {
		return "<end>"
	}

	s := p.tokens[p.pos]
	p.pos++
	return s
}

func (p *filterParser) or() (traceFilter, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}

	for p.take("OR") {
		right, e := p.and()
		if e != nil {
			return nil, e
		}

		prev := left
		left = func(t traceRecord) bool { return prev(t) || right(t) }
	}

	return left, nil
}

func (p *filterParser) and() (traceFilter, error) {
	left, err := p.atom()
	if err != nil {
		return nil, err
	}

	for p.take("AND") {
		right, e := p.atom()
		if e != nil {
			return nil, e
		}

		prev := left
		left = func(t traceRecord) bool { return prev(t) && right(t) }
	}

	return left, nil
}

func (p *filterParser) atom() (traceFilter, error) {
	if p.take("(") {
		f, err := p.or()
		if err != nil {
			return nil, err
		}

		if !p.take(")") {
			return nil, invalid("unsupported filter part: expected )")
		}

		return f, nil
	}

	key := p.next()
	switch key {
	case "service":
		if !p.take("(") {
			return nil, invalid("unsupported service filter: expected (")
		}

		name, e := strconv.Unquote(p.next())
		if e != nil || !p.take(")") {
			return nil, invalid("unsupported service filter: expected quoted name and )")
		}

		return anyDocument(func(d *segmentDocument) bool { return d.Name == name }), nil
	case "error", "fault", "throttle":
		return anyDocument(func(d *segmentDocument) bool {
			switch key {
			case "error":
				return d.Error
			case "fault":
				return d.Fault
			default:
				return d.Throttle
			}
		}), nil
	case filterHTTP, "annotation":
		if !p.take(".") {
			return nil, invalid("unsupported filter part %q", key)
		}

		field := p.next()
		if (key == filterHTTP && field != "status") || !annotationKey.MatchString(field) || !p.take("=") {
			return nil, invalid("unsupported filter part %q", key+"."+field)
		}

		literal := p.next()
		if literal == "-" {
			literal += p.next()
		}

		var value any
		switch {
		case strings.HasPrefix(literal, "\""):
			v, e := strconv.Unquote(literal)
			if e != nil {
				return nil, invalid("unsupported filter value %q", literal)
			}

			value = v
		case literal == "true" || literal == "false":
			value = literal == "true"
		default:
			v, e := strconv.ParseFloat(literal, 64)
			if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, invalid("unsupported filter value %q", literal)
			}

			value = v
		}

		if key == filterHTTP {
			v, ok := value.(float64)
			if !ok || v < 100 || v > 599 || v != float64(int64(v)) {
				return nil, invalid("unsupported http.status value %q", literal)
			}

			return anyDocument(func(d *segmentDocument) bool { return float64(d.status()) == v }), nil
		}

		return anyDocument(func(d *segmentDocument) bool { return d.Annotations[field] == value }), nil
	case "responsetime", "duration":
		if !p.take(">") {
			return nil, invalid("unsupported %s operator: expected >", key)
		}

		value, e := strconv.ParseFloat(p.next(), 64)
		if e != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, invalid("unsupported %s value", key)
		}

		return func(t traceRecord) bool {
			summary := t.summary()
			if key == "duration" {
				return *summary.Duration > value
			}

			return *summary.ResponseTime > value
		}, nil
	default:
		return nil, invalid("unsupported filter part %q", key)
	}
}

func anyDocument(predicate func(*segmentDocument) bool) traceFilter {
	return func(t traceRecord) bool {
		for _, d := range t.documents() {
			if predicate(d) {
				return true
			}
		}

		return false
	}
}
