package service

import (
	"context"
	"net/http"

	"karl/pkg/config"
	"karl/pkg/model"
)

var (
	_ Client           = (*defaultService)(nil)
	_ VariantExtractor = (*defaultService)(nil)
	_ Fingerprinter    = (*defaultService)(nil)
)

type defaultService struct {
	config     *config.AppConfig
	httpClient *http.Client
}

func newDefaultService(config *config.AppConfig, httpClient *http.Client) Client {
	return &defaultService{config: config, httpClient: httpClient}
}

func (c *defaultService) ID() ID {
	return "default"
}

func (c *defaultService) ExtractVariants(ctx context.Context, reference model.Reference) ([]model.Variant, error) {
	return NewDefaultVariantExtractor(c.config, c.httpClient, "").ExtractVariants(ctx, reference)
}

func (c *defaultService) Fingerprint(ctx context.Context, variant model.Variant) (model.Fingerprint, error) {
	return NewDefaultFingerprinter(c.config, c.httpClient, "").Fingerprint(ctx, variant)
}
