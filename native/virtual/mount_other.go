//go:build !linux

package virtual

import (
	"context"
	"fmt"
	bough "github.com/scholzdev/boughline/native"
)

func Mount(context.Context, *bough.Client, string, []string, string, string) error {
	return fmt.Errorf("virtual checkouts require Linux with FUSE; ordinary sparse checkouts work on this platform")
}
