//go:build clientonly

package main

import (
	"context"
	"errors"
)

// The downloadable client leaves out the hub and its database, which makes it a fraction of
// the size. Build with -tags clientonly.
var errClientOnly = errors.New("this is the client only build, the hub commands are in the docker image")

func runServe(context.Context) error           { return errClientOnly }
func runAdmin(context.Context, []string) error { return errClientOnly }
func runHealthcheck() error                    { return errClientOnly }
