package main

import (
	"errors"
	"net"
	"net/http"
)

// A missing write callback alone is not proof of an unsent request: custom
// transports and ambiguous read failures may not provide those callbacks.
func definitelyUnsentUpstreamError(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func unchargedUpstreamStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}
