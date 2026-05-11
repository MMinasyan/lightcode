// Package provider builds an OpenAI-compatible HTTP client for any
// configured provider. This file previously held the go-openai SDK wrapper;
// it has been replaced by a custom HTTP/SSE client in client.go, request.go,
// stream.go, types.go, and errors.go.
package provider
