//go:build !linux

package main

import (
	"context"
	"errors"
)

// run needs Linux namespaces; the program serves only Linux nodes.
func run(ctx context.Context, argv ...string) (string, error) {
	return "", errors.New("disk runs node programs only on linux")
}
