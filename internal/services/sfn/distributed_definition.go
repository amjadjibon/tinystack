package sfn

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

type mapReader struct {
	Parameters   any
	Arguments    any
	ReaderConfig *readerConfig
	Resource     string
}

type readerConfig struct {
	InputType         string
	CSVHeaderLocation string
	CSVDelimiter      string
	MaxItemsPath      string
	CSVHeaders        []string
	MaxItems          number
}

type mapWriter struct {
	Parameters   any
	Arguments    any
	WriterConfig *writerConfig
	Resource     string
}

type writerConfig struct {
	OutputType     string
	Transformation string
}

type mapBatcher struct {
	BatchInput                any
	MaxItemsPerBatchPath      string
	MaxInputBytesPerBatchPath string
	MaxItemsPerBatch          number
	MaxInputBytesPerBatch     number
}

func (s *state) distributed() bool {
	return s.ItemProcessor != nil && s.ItemProcessor.ProcessorConfig != nil &&
		s.ItemProcessor.ProcessorConfig.Mode == "DISTRIBUTED"
}

var mapLabel = regexp.MustCompile("^[^ <>{}\\[\\]?*\"#%\\\\^|~`$&,;:\\x00-\\x1f\\x7f-\\x9f]+$")

// validateDistributed follows https://docs.aws.amazon.com/step-functions/latest/dg/state-map-distributed.html.
func (s *state) validateDistributed(at string, add func(string, ...any)) {
	bad := func(msg string) { add("SCHEMA_VALIDATION_FAILED: %s at %s", msg, at) }
	if p := s.ItemProcessor; p != nil && p.ProcessorConfig != nil {
		c := p.ProcessorConfig
		if c.Mode != "" && c.Mode != "INLINE" && c.Mode != "DISTRIBUTED" {
			bad("ProcessorConfig.Mode must be INLINE or DISTRIBUTED")
		}

		if s.distributed() {
			if c.ExecutionType != typeStandard && c.ExecutionType != typeExpress {
				bad("Distributed ItemProcessor requires ExecutionType STANDARD or EXPRESS")
			}
		} else if c.ExecutionType != "" {
			bad("ExecutionType requires DISTRIBUTED mode")
		}
	}

	if !s.distributed() {
		if s.MaxConcurrencyPath != "" {
			bad("TinyStack supports MaxConcurrencyPath only in DISTRIBUTED mode")
		}

		if s.Label != "" || s.ToleratedFailureCount.set || s.ToleratedFailurePercentage.set ||
			s.ToleratedFailureCountPath != "" || s.ToleratedFailurePercentagePath != "" {
			bad("Label and tolerated failure thresholds require DISTRIBUTED mode")
		}

		return
	}

	if s.ItemsPath != "" {
		steps, err := parsePath(s.ItemsPath)
		if err != nil || !definite(steps) {
			bad("ItemsPath must be a reference path")
		}
	}

	if s.ItemReader != nil && (s.ItemsPath != "" || s.Items != nil) {
		bad("ItemsPath and Items cannot be combined with ItemReader")
	}

	if s.ItemProcessor.ProcessorConfig.ExecutionType == typeExpress && s.ItemProcessor.containsDistributed() {
		bad("Distributed Map requires a STANDARD parent")
	}

	if s.Iterator != nil {
		bad("Distributed Map cannot specify Iterator")
	}

	if s.Parameters != nil {
		bad("Distributed Map uses ItemSelector instead of Parameters")
	}

	invalidLabel := !mapLabel.MatchString(s.Label) || utf8.RuneCountInString(s.Label) > 40 ||
		strings.ContainsFunc(s.Label, unicode.IsSpace)
	if s.Label != "" && invalidLabel {
		bad("Label must contain at most 40 characters without whitespace or reserved characters")
	}

	s.checkMapNumber(s.MaxConcurrency, s.MaxConcurrencyPath, "MaxConcurrency", 0, 10000, true, at, add)
	s.checkMapNumber(
		s.ToleratedFailureCount,
		s.ToleratedFailureCountPath,
		"ToleratedFailureCount",
		0,
		math.MaxInt64,
		true,
		at,
		add,
	)
	s.checkMapNumber(s.ToleratedFailurePercentage, s.ToleratedFailurePercentagePath,
		"ToleratedFailurePercentage", 0, 100, false, at, add)
	if b := s.ItemBatcher; b != nil {
		s.checkMapNumber(
			b.MaxItemsPerBatch,
			b.MaxItemsPerBatchPath,
			"MaxItemsPerBatch",
			1,
			math.MaxInt64,
			true,
			at,
			add,
		)
		s.checkMapNumber(b.MaxInputBytesPerBatch, b.MaxInputBytesPerBatchPath,
			"MaxInputBytesPerBatch", 1, maxInput, true, at, add)
		if !b.MaxItemsPerBatch.set && b.MaxItemsPerBatchPath == "" &&
			!b.MaxInputBytesPerBatch.set && b.MaxInputBytesPerBatchPath == "" {
			bad("ItemBatcher requires MaxItemsPerBatch or MaxInputBytesPerBatch")
		}

		if s.jsonata {
			checkJSONata(b.BatchInput, at+"/ItemBatcher/BatchInput", add)
		}
	}

	if rd := s.ItemReader; rd != nil {
		switch rd.Resource {
		case "arn:aws:states:::s3:getObject":
			if rd.ReaderConfig == nil ||
				!slices.Contains([]string{inputJSON, inputJSONL, inputCSV}, rd.ReaderConfig.InputType) {
				bad("TinyStack ItemReader supports JSON, JSONL and CSV getObject inputs")
			}
		case "arn:aws:states:::s3:listObjectsV2":
			if rd.ReaderConfig != nil && rd.ReaderConfig.InputType != "" {
				bad("InputType requires getObject")
			}
		default:
			bad("Unsupported ItemReader Resource")
		}

		s.checkMapArguments(rd.Parameters, rd.Arguments, at+"/ItemReader", add)
		if c := rd.ReaderConfig; c != nil {
			s.checkMapNumber(c.MaxItems, c.MaxItemsPath, "MaxItems", 1, 100000000, true, at, add)
			if c.InputType == inputCSV {
				if c.CSVHeaderLocation != "FIRST_ROW" && c.CSVHeaderLocation != headerGiven {
					bad("CSV requires CSVHeaderLocation FIRST_ROW or GIVEN")
				}

				if c.CSVHeaderLocation == headerGiven && len(c.CSVHeaders) == 0 {
					bad("GIVEN requires CSVHeaders")
				}

				if c.CSVHeaderLocation != headerGiven && c.CSVHeaders != nil {
					bad("CSVHeaders requires GIVEN")
				}

				if !slices.Contains([]string{"", "COMMA", "PIPE", "SEMICOLON", "SPACE", "TAB"}, c.CSVDelimiter) {
					bad("Unsupported CSVDelimiter")
				}
			} else if c.CSVHeaderLocation != "" || c.CSVHeaders != nil || c.CSVDelimiter != "" {
				bad("CSV options require CSV InputType")
			}
		}
	}

	if w := s.ResultWriter; w != nil {
		// WriterConfig-only preview is valid in AWS; Resource is required only for S3 export.
		// https://docs.aws.amazon.com/step-functions/latest/dg/input-output-resultwriter.html
		if w.Resource == "" {
			if w.WriterConfig == nil || w.Parameters != nil || w.Arguments != nil {
				bad("ResultWriter requires Resource for S3 export or WriterConfig for preview")
			}
		} else {
			if w.Resource != "arn:aws:states:::s3:putObject" {
				bad("Unsupported ResultWriter Resource")
			}

			s.checkMapArguments(w.Parameters, w.Arguments, at+"/ResultWriter", add)
		}

		if c := w.WriterConfig; c != nil {
			if !slices.Contains([]string{"", inputJSON, inputJSONL}, c.OutputType) {
				bad("Unsupported ResultWriter OutputType")
			}

			if !slices.Contains([]string{"", transformNone, "COMPACT", "FLATTEN"}, c.Transformation) {
				bad("Unsupported ResultWriter Transformation")
			}
		}
	}
}

func (s *state) checkMapArguments(params, args any, at string, add func(string, ...any)) {
	if s.jsonata {
		if params != nil || args == nil {
			add("SCHEMA_VALIDATION_FAILED: JSONata requires Arguments at %s", at)
		}

		checkJSONata(args, at+"/Arguments", add)
	} else if args != nil || params == nil {
		add("SCHEMA_VALIDATION_FAILED: JSONPath requires Parameters at %s", at)
	}
}

func (s *state) checkMapNumber(n number, path, name string, minValue, maxValue float64,
	integer bool, at string, add func(string, ...any),
) {
	if n.set && path != "" {
		add("SCHEMA_VALIDATION_FAILED: %s and %sPath are mutually exclusive at %s", name, name, at)
	}

	if path != "" {
		steps, err := parsePath(path)
		if s.jsonata || err != nil || !definite(steps) {
			add("SCHEMA_VALIDATION_FAILED: %sPath requires a JSONPath reference path at %s", name, at)
		}
	}

	if n.expr != "" {
		if !s.jsonata {
			add("SCHEMA_VALIDATION_FAILED: %s expressions require JSONata at %s", name, at)
		}

		checkJSONata(n.expr, at+"/"+name, add)
	} else if n.set && (n.value < minValue || n.value > maxValue || (integer && math.Trunc(n.value) != n.value)) {
		add("SCHEMA_VALIDATION_FAILED: Invalid %s at %s", name, at)
	}
}

func (p *processor) UnmarshalJSON(data []byte) error {
	type plain processor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*plain)(p)); err != nil {
		return err
	}

	var definition map[string]json.RawMessage
	if err := json.Unmarshal(data, &definition); err != nil {
		return err
	}

	delete(definition, "ProcessorConfig")
	var err error
	p.raw, err = json.Marshal(definition)
	return err
}

const (
	fieldItems    = "Items"
	fieldItem     = "Item"
	fieldInput    = "Input"
	errDataLimit  = "States.DataLimitExceeded"
	transformNone = "NONE"
	inputCSV      = "CSV"
	inputJSON     = "JSON"
	inputJSONL    = "JSONL"
	headerGiven   = "GIVEN"
	fieldKey      = "Key"
)

func (m *machine) containsDistributed() bool {
	for _, s := range m.States {
		if s.distributed() {
			return true
		}

		for _, branch := range s.Branches {
			if branch.containsDistributed() {
				return true
			}
		}

		if s.Iterator != nil && s.Iterator.containsDistributed() {
			return true
		}

		if s.ItemProcessor != nil && s.ItemProcessor.containsDistributed() {
			return true
		}
	}

	return false
}
