// Package provider builds an OpenAI-compatible HTTP client for any
// configured provider. Every provider in Lightcode speaks the OpenAI API
// schema; the only per-provider variation is base URL, API key, and
// model-level options.
package provider

import (
	"net/http"

	"github.com/MMinasyan/lightcode/internal/catalog"
)

// Client is a thin HTTP client that talks to an OpenAI-compatible endpoint
// using catalog-resolved provider and model metadata.
type Client struct {
	provider *catalog.Provider
	model    *catalog.Model
	apiKey   string
	http     *http.Client
}
