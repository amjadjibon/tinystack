package cloudwatch

import (
	"strings"
)

const (
	maxRule    = 10240
	opConstant = "CONST"
)

// rule is a parsed composite alarm rule: an operator over its children, a
// constant, or a state check of another alarm.
type rule struct {
	op       string
	state    string
	alarm    string
	children []*rule
	constant bool
}

// evaluate reports whether the rule holds, given each alarm's state.
func (r *rule) evaluate(state func(name string) string) bool {
	switch r.op {
	case "AND":
		for _, c := range r.children {
			if !c.evaluate(state) {
				return false
			}
		}

		return true
	case "OR":
		for _, c := range r.children {
			if c.evaluate(state) {
				return true
			}
		}

		return false
	case "NOT":
		return !r.children[0].evaluate(state)
	case opConstant:
		return r.constant
	}

	return state(r.alarm) == r.state
}

// alarms lists the alarms the rule refers to.
func (r *rule) alarms() []string {
	if r.op == "STATE" {
		return []string{r.alarm}
	}

	var out []string
	for _, c := range r.children {
		out = append(out, c.alarms()...)
	}

	return out
}

type ruleParser struct {
	src string
	pos int
}

// parseRule parses an AlarmRule: ALARM(x), OK(x), and INSUFFICIENT_DATA(x) of
// an alarm name (quoted, or bare) or ARN, TRUE, FALSE, AND, OR, NOT, and
// parentheses.
func parseRule(src string) (*rule, error) {
	if src == "" {
		return nil, missing("AlarmRule")
	}

	if len(src) > maxRule {
		return nil, invalidValue("The AlarmRule must be at most %d characters", maxRule)
	}

	p := &ruleParser{src: src}
	r, err := p.or()
	if err != nil {
		return nil, err
	}

	p.skip()
	if p.pos != len(src) {
		return nil, p.fail()
	}

	return r, nil
}

func (p *ruleParser) fail() error {
	return invalidValue("Could not parse the AlarmRule near position %d", p.pos+1)
}

func (p *ruleParser) skip() {
	for p.pos < len(p.src) && strings.ContainsRune(" \t\r\n", rune(p.src[p.pos])) {
		p.pos++
	}
}

// word consumes the keyword w when it comes next as a whole word.
func (p *ruleParser) word(w string) bool {
	p.skip()
	rest := p.src[p.pos:]
	if !strings.HasPrefix(rest, w) || len(rest) > len(w) && isWord(rest[len(w)]) {
		return false
	}

	p.pos += len(w)
	return true
}

func (p *ruleParser) or() (*rule, error) {
	return p.chain("OR", p.and)
}

func (p *ruleParser) and() (*rule, error) {
	return p.chain("AND", p.not)
}

func (p *ruleParser) chain(op string, next func() (*rule, error)) (*rule, error) {
	first, err := next()
	if err != nil {
		return nil, err
	}

	children := []*rule{first}
	for p.word(op) {
		c, cErr := next()
		if cErr != nil {
			return nil, cErr
		}

		children = append(children, c)
	}

	if len(children) == 1 {
		return first, nil
	}

	return &rule{op: op, children: children}, nil
}

func (p *ruleParser) not() (*rule, error) {
	if p.word("NOT") {
		c, err := p.not()
		if err != nil {
			return nil, err
		}

		return &rule{op: "NOT", children: []*rule{c}}, nil
	}

	return p.atom()
}

func (p *ruleParser) atom() (*rule, error) {
	switch {
	case p.word("TRUE"):
		return &rule{op: opConstant, constant: true}, nil
	case p.word("FALSE"):
		return &rule{op: opConstant}, nil
	}

	p.skip()
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		p.pos++
		r, err := p.or()
		if err != nil {
			return nil, err
		}

		p.skip()
		if p.pos == len(p.src) || p.src[p.pos] != ')' {
			return nil, p.fail()
		}

		p.pos++
		return r, nil
	}

	for _, state := range []string{stateAlarm, stateOK, stateNoData} {
		if !p.word(state) {
			continue
		}

		p.skip()
		if p.pos == len(p.src) || p.src[p.pos] != '(' {
			return nil, p.fail()
		}

		p.pos++
		name, err := p.name()
		if err != nil {
			return nil, err
		}

		return &rule{op: "STATE", state: state, alarm: name}, nil
	}

	return nil, p.fail()
}

// name reads an alarm name or ARN and the closing parenthesis.
func (p *ruleParser) name() (string, error) {
	p.skip()
	var name string
	if p.pos < len(p.src) && p.src[p.pos] == '"' {
		end := strings.IndexByte(p.src[p.pos+1:], '"')
		if end < 0 {
			return "", p.fail()
		}

		name, p.pos = p.src[p.pos+1:p.pos+1+end], p.pos+end+2
		p.skip()
	} else {
		end := strings.IndexByte(p.src[p.pos:], ')')
		if end < 0 {
			return "", p.fail()
		}

		name, p.pos = strings.TrimSpace(p.src[p.pos:p.pos+end]), p.pos+end
	}

	if p.pos == len(p.src) || p.src[p.pos] != ')' || name == "" {
		return "", p.fail()
	}

	p.pos++
	if _, after, ok := strings.Cut(name, ":alarm:"); ok && strings.HasPrefix(name, "arn:") {
		name = after
	}

	return name, nil
}
