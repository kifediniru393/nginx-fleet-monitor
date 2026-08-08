//go:build !linux

package vrrp

import (
	"context"
	"errors"
)

// Listen is Linux-only (AF_PACKET). On other platforms the vrrp collector
// reports itself disabled rather than failing the exporter.
func Listen(ctx context.Context, tr *Tracker) error {
	return errors.New("vrrp: passive listener requires Linux (AF_PACKET)")
}
