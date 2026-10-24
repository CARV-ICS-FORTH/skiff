//go:build !linux

package direct

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("the direct path needs Linux")

// ServeFactory is not available here.
func ServeFactory(context.Context, string) error { return errUnsupported }

// FactoryClient is not available here.
type FactoryClient struct{}

// NewFactoryClient makes a client that cannot be used.
func NewFactoryClient(string) *FactoryClient { return &FactoryClient{} }

// Close does nothing.
func (*FactoryClient) Close() {}
