package main

import (
	"errors"
	"net/http"
)

func writeConversionLimitError(w http.ResponseWriter, r *http.Request, err error) bool {
	var sizeError *conversionSizeError
	if !errors.As(err, &sizeError) {
		return false
	}
	writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", sizeError.Error())
	return true
}
