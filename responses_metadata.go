package main

import (
	"context"
	"net/http"
)

type responsesMetadataContextKey struct{}

// Metadata belongs to the Responses resource. It never needs to reach Chat.
// Copy both on capture and output so envelope mutations cannot cross requests.
func withResponsesMetadata(r *http.Request, metadata any) *http.Request {
	values, ok := metadata.(map[string]any)
	if !ok {
		return r
	}
	copy := make(map[string]any, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return r.WithContext(context.WithValue(r.Context(), responsesMetadataContextKey{}, copy))
}

func buildResponsesEnvelopeForRequest(r *http.Request, id, model string, createdAt int64) map[string]any {
	envelope := buildResponsesEnvelope(id, model, createdAt)
	if r != nil {
		if metadata, ok := r.Context().Value(responsesMetadataContextKey{}).(map[string]any); ok {
			copy := make(map[string]any, len(metadata))
			for key, value := range metadata {
				copy[key] = value
			}
			envelope["metadata"] = copy
		}
	}
	return envelope
}
