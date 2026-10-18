//go:build !linux

package direct

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("the direct path needs Linux")

// ServeFactory is not available here.
func ServeFactory(context.Context, string) error { return errUnsupported }
