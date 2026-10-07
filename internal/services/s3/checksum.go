package s3

import (
	"encoding/base64"
	"net/http"
	"reflect"
	"strconv"

	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	defaultChecksum = "CRC64NVME"
	typeFullObject  = "FULL_OBJECT"
	typeComposite   = "COMPOSITE"
	modeEnabled     = "ENABLED"
)

// checksum is the stored checksum of an object or, with no Type, of one part.
// Value is the base64 digest S3 reports; a composite value ends in "-<parts>".
type checksum struct {
	Algorithm string
	Value     string
	Type      string `json:",omitempty"`
}

// sum returns the base64 digest of data under a supported algorithm.
func sum(algorithm string, data []byte) string {
	h := checksums()[algorithm]()
	_, _ = h.Write(data)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// memberString reads the string member name of a generated request, or "".
func memberString(in any, name string) string {
	f := reflect.ValueOf(in).Elem().FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.Pointer || f.IsNil() {
		return ""
	}

	return f.Elem().String()
}

// sentChecksums returns the x-amz-checksum-<algorithm> values a request
// carries, keyed by algorithm.
func sentChecksums(in any) map[string]string {
	sent := map[string]string{}
	for algorithm := range checksums() {
		if v := memberString(in, "Checksum"+algorithm); v != "" {
			sent[algorithm] = v
		}
	}

	return sent
}

// validAlgorithm rejects an algorithm TinyStack cannot compute.
func validAlgorithm(algorithm string) error {
	if algorithm != "" && checksums()[algorithm] == nil {
		return fail(http.StatusBadRequest, "InvalidRequest", "Checksum algorithm "+algorithm+" is not supported")
	}

	return nil
}

// checksumOf decides the checksum an upload of body stores. A request names
// its algorithm with ChecksumAlgorithm or by sending one value; expect is the
// algorithm an upload fixed, if any. With none named the default is CRC64NVME,
// as S3 computes. A sent value must match the body.
func checksumOf(in any, body []byte, expect string) (checksum, error) {
	sent := sentChecksums(in)
	if len(sent) > 1 {
		return checksum{}, fail(http.StatusBadRequest, "InvalidRequest",
			"Expecting a single x-amz-checksum- header. Multiple checksum Types are not allowed.")
	}

	algorithm := memberString(in, "ChecksumAlgorithm")
	if err := validAlgorithm(algorithm); err != nil {
		return checksum{}, err
	}

	for name := range sent {
		if algorithm != "" && algorithm != name {
			return checksum{}, fail(http.StatusBadRequest, "InvalidRequest",
				"Checksum Type mismatch occurred, expected checksum Type: "+algorithm+", actual checksum Type: "+name)
		}

		algorithm = name
	}

	if expect != "" {
		if algorithm != "" && algorithm != expect {
			return checksum{}, fail(http.StatusBadRequest, "InvalidRequest",
				"Checksum Type mismatch occurred, expected checksum Type: "+expect+", actual checksum Type: "+algorithm)
		}

		algorithm = expect
	}

	if algorithm == "" {
		algorithm = defaultChecksum
	}

	c := checksum{Algorithm: algorithm, Value: sum(algorithm, body)}
	if want, ok := sent[algorithm]; ok && want != c.Value {
		return checksum{}, fail(http.StatusBadRequest, "BadDigest",
			"The "+algorithm+" you specified did not match the calculated checksum.")
	}

	return c, nil
}

// uploadType picks a multipart upload's algorithm and checksum type from
// CreateMultipartUpload: CRC64NVME is always FULL_OBJECT, SHA-1 and SHA-256
// are always COMPOSITE, and CRC32 and CRC32C are COMPOSITE unless asked for
// FULL_OBJECT. With no algorithm the upload uses the default.
func uploadType(algorithm, kind *string) (algo, typ string, err error) {
	algo, typ = common.Value(algorithm), common.Value(kind)
	if algoErr := validAlgorithm(algo); algoErr != nil {
		return "", "", algoErr
	}

	if typ != "" && typ != typeFullObject && typ != typeComposite {
		return "", "", invalidArgument("Invalid checksum type " + typ)
	}

	if algo == "" && typ != "" {
		return "", "", fail(http.StatusBadRequest, "InvalidRequest",
			"The x-amz-checksum-type header can only be used with the x-amz-checksum-algorithm header.")
	}

	switch {
	case algo == "":
		return defaultChecksum, typeFullObject, nil
	case typ == "" && algo == defaultChecksum:
		typ = typeFullObject
	case typ == "":
		typ = typeComposite
	}

	if algo == defaultChecksum && typ == typeComposite ||
		(algo == "SHA1" || algo == "SHA256") && typ == typeFullObject {
		return "", "", fail(http.StatusBadRequest, "InvalidRequest",
			"The "+typ+" checksum type is not supported with the "+algo+" algorithm.")
	}

	return algo, typ, nil
}

// compositeOf is the checksum of a COMPOSITE multipart upload: the digest of
// the concatenated raw part digests, with the part count as a suffix.
func compositeOf(algorithm string, parts []string) (string, error) {
	var raw []byte
	for _, p := range parts {
		digest, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return "", err
		}

		raw = append(raw, digest...)
	}

	return sum(algorithm, raw) + "-" + strconv.Itoa(len(parts)), nil
}

// apply sets the checksum members of a generated output: Checksum<algorithm>
// and, when the shape has it, ChecksumType.
func (c *checksum) apply(out any) {
	if c == nil || c.Value == "" {
		return
	}

	v := reflect.ValueOf(out).Elem()
	if f := v.FieldByName("Checksum" + c.Algorithm); f.IsValid() {
		f.Set(reflect.ValueOf(new(c.Value)))
	}

	if f := v.FieldByName("ChecksumType"); f.IsValid() && c.Type != "" {
		f.Set(reflect.ValueOf(new(c.Type)))
	}
}

// reporting reports whether ChecksumMode asks for the stored checksum.
func reporting(mode *string) (bool, error) {
	switch m := common.Value(mode); m {
	case "":
		return false, nil
	case modeEnabled:
		return true, nil
	default:
		return false, invalidArgument("Invalid checksum mode " + m)
	}
}
