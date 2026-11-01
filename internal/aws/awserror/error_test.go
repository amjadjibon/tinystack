package awserror_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

func TestFrom(t *testing.T) {
	cases := []struct {
		err    error
		code   string
		status int
	}{
		{awserror.New("NoSuchBucket", "missing"), "NoSuchBucket", 400},
		{fmt.Errorf("list: %w", context.Canceled), "RequestCanceled", 499},
		{errors.New("disk full"), "InternalServerError", 500},
	}
	for _, tc := range cases {
		e := awserror.From(tc.err)
		if e.Code != tc.code || e.StatusCode != tc.status {
			t.Errorf("From(%v) = %s %d, want %s %d", tc.err, e.Code, e.StatusCode, tc.code, tc.status)
		}
	}
}
