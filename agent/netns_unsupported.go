//go:build !linux

package agent

import (
	"context"
	"errors"
	"net"
)

var errNetns = errors.New("cannot enter network namespace")

func dialInNetns(ctx context.Context, nsPath, network, addr string) (net.Conn, error) {
	return nil, errNetns
}
