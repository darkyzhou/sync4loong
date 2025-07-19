package utils

import (
	"context"
	"net"

	"golang.org/x/crypto/ssh"
)

func SSHDialContext(ctx context.Context, network, addr string, config *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: config.Timeout}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}

	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result)
	go func() {
		var client *ssh.Client
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
		if err == nil {
			client = ssh.NewClient(c, chans, reqs)
		}
		select {
		case ch <- result{client, err}:
		case <-ctx.Done():
			if client != nil {
				client.Close()
			}
		}
	}()
	select {
	case res := <-ch:
		return res.client, res.err
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}
