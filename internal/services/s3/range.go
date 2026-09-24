package s3

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// byteRange is an inclusive interval into an object.
type byteRange struct{ start, end int64 }

func invalidRange(size int64) error {
	return &awserror.Error{
		Code:       "InvalidRange",
		Message:    "The requested range cannot be satisfied",
		StatusCode: http.StatusRequestedRangeNotSatisfiable,
		Headers:    map[string]string{"Content-Range": fmt.Sprintf("bytes */%d", size)},
	}
}

// parseRange accepts one HTTP byte range. S3 does not serve multipart ranges.
func parseRange(spec string, size int64) (byteRange, error) {
	unit, value, ok := strings.Cut(spec, "=")
	if !ok || !strings.EqualFold(unit, "bytes") || value == "" || strings.Contains(value, ",") || size == 0 {
		return byteRange{}, invalidRange(size)
	}

	first, last, ok := strings.Cut(value, "-")
	if !ok || (first == "" && last == "") {
		return byteRange{}, invalidRange(size)
	}

	if first == "" {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return byteRange{}, invalidRange(size)
		}

		return byteRange{start: max(0, size-suffix), end: size - 1}, nil
	}

	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return byteRange{}, invalidRange(size)
	}

	end := size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return byteRange{}, invalidRange(size)
		}

		end = min(end, size-1)
	}

	return byteRange{start: start, end: end}, nil
}
