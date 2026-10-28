package main

import (
	"net/http"
	"sync"
)

// Each safe retry owns its connections. Copy the client's policies (including
// cookies, proxy/TLS and redirects); never close the shared idle pool on error.
func isolatedRetryClient(base *http.Client) (*http.Client, func()) {
	var transport *http.Transport
	switch t := base.Transport.(type) {
	case nil:
		standard, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return base, nil
		}
		transport = standard.Clone()
	case *http.Transport:
		transport = t.Clone()
	case interface{ freshRetryTransport() *http.Transport }:
		transport = t.freshRetryTransport()
	default:
		// Arbitrary custom RoundTrippers cannot safely be cloned. Their own
		// pooling remains their responsibility; Request.Close still applies.
		return base, nil
	}
	transport.DisableKeepAlives = true
	client := *base
	client.Transport = transport
	var once sync.Once
	return &client, func() { once.Do(transport.CloseIdleConnections) }
}

func (t *hotConfigTransport) freshRetryTransport() *http.Transport {
	header, _, _ := upstreamSettings()
	t.mu.Lock()
	clone := t.transport.Clone()
	t.mu.Unlock()
	clone.ResponseHeaderTimeout = header
	return clone
}
