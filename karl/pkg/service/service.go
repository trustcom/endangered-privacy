package service

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	"karl/pkg/config"
	"karl/pkg/model"
)

type ID = string

type (
	Client interface {
		ID() ID
	}

	Constructor func(config *config.AppConfig, httpClient *http.Client) Client

	URLExtractor interface {
		ExtractURLs(ctx context.Context) ([]string, error)
	}

	VideoExtractor interface {
		Matches(url string) bool
		VideoExtract(ctx context.Context, url string) []model.VideoResult
	}

	VariantExtractor interface {
		ExtractVariants(ctx context.Context, reference model.Reference) ([]model.Variant, error)
	}

	Fingerprinter interface {
		Fingerprint(ctx context.Context, variant model.Variant) (model.Fingerprint, error)
	}
)

type Manager struct {
	config            *config.AppConfig
	httpClient        *http.Client
	clients           map[ID]Client
	urlExtractors     map[ID]URLExtractor
	videoExtractors   map[ID]VideoExtractor
	variantExtractors map[ID]VariantExtractor
	fingerprinters    map[ID]Fingerprinter
}

func NewManager(httpClient *http.Client, config *config.AppConfig) *Manager {
	m := &Manager{
		config:            config,
		httpClient:        httpClient,
		clients:           make(map[ID]Client),
		urlExtractors:     make(map[ID]URLExtractor),
		videoExtractors:   make(map[ID]VideoExtractor),
		variantExtractors: make(map[ID]VariantExtractor),
		fingerprinters:    make(map[ID]Fingerprinter),
	}

	m.register(newDefaultService)

	return m
}

func (m *Manager) Register(constructor Constructor) {
	m.register(constructor)
}

func (m *Manager) register(constructor Constructor) ID {
	var (
		c  = constructor(m.config, m.httpClient)
		id = c.ID()
	)

	if _, ok := m.clients[id]; ok {
		log.Fatalf("%q already registered", id)
	}

	m.clients[id] = c

	if ue, ok := c.(URLExtractor); ok {
		m.urlExtractors[id] = ue
	}

	if e, ok := c.(VideoExtractor); ok {
		m.videoExtractors[id] = e
	}

	if ve, ok := c.(VariantExtractor); ok {
		m.variantExtractors[id] = ve
	}

	if f, ok := c.(Fingerprinter); ok {
		m.fingerprinters[id] = f
	}

	return id
}

func (m *Manager) matchURL(u string) (ID, bool) {
	for id, ve := range m.videoExtractors {
		if ve.Matches(u) {
			return id, true
		}
	}
	return "", false
}

func (m *Manager) ExtractURLs(ctx context.Context, service ID) (model.URLExtractResult, error) {
	ue, ok := m.urlExtractors[service]
	if !ok {
		return model.URLExtractResult{}, fmt.Errorf("%q not URL extractor", service)
	}

	urls, err := ue.ExtractURLs(ctx)
	if err != nil {
		return model.URLExtractResult{}, fmt.Errorf("extract urls: %w", err)
	}

	return model.URLExtractResult{
		Service: service,
		URLs:    urls,
	}, nil
}

func (m *Manager) Extract(ctx context.Context, pg *errgroup.Group, url, format string) (model.ExtractResult, error) {
	id, ok := m.matchURL(url)
	if !ok {
		return model.ExtractResult{}, fmt.Errorf("%q missing video extractor", url)
	}

	result := model.ExtractResult{
		URL:     url,
		Service: id,
	}

	var (
		pMu sync.Mutex
		wg  sync.WaitGroup
	)
	for _, r := range m.videoExtractors[id].VideoExtract(ctx, url) {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		pg.Go(func() error {
			defer wg.Done()
			if r.Err != nil {
				result.NumFailed++
				result.FailedErrors = append(result.FailedErrors, fmt.Errorf("video extract %q: %w", url, r.Err))
				return nil
			}

			var (
				vid       = r.Video
				parentCtx = ctx
				variants  []model.Variant
				mu        sync.Mutex
			)
			g, ctx := errgroup.WithContext(parentCtx)
			for _, ref := range r.References {
				if format != "both" && ref.Format != format {
					continue
				}

				g.Go(func() error {
					vs, err := m.extractVariants(ctx, id, ref)
					if err == nil {
						mu.Lock()
						variants = append(variants, vs...)
						mu.Unlock()
					}
					return err
				})
			}
			if err := g.Wait(); err != nil {
				result.NumFailed++
				result.FailedErrors = append(result.FailedErrors, fmt.Errorf("extract variants %q: %w", url, err))
				return nil
			}

			seen := make(map[string]struct{})
			g, ctx = errgroup.WithContext(parentCtx)
			for _, v := range variants {
				if _, ok := seen[v.ID]; ok {
					continue
				}
				seen[v.ID] = struct{}{}
				g.Go(func() error {
					err := m.fingerprint(ctx, id, &v)
					if err == nil {
						mu.Lock()
						vid.Variants = append(vid.Variants, v)
						mu.Unlock()
					}
					return err
				})
			}
			if err := g.Wait(); err != nil {
				result.NumFailed++
				result.FailedErrors = append(result.FailedErrors, fmt.Errorf("fingerprint %q: %w", url, err))
				return nil
			}

			pMu.Lock()
			result.Videos = append(result.Videos, vid)
			pMu.Unlock()
			return nil
		})
	}
	wg.Wait()

	if len(result.Videos) == 0 {
		return model.ExtractResult{}, fmt.Errorf("extract %q: no fingerprints", url)
	}

	return result, nil
}

func (m *Manager) Fingerprint(ctx context.Context, fileOrURL, baseURL, indexRange string) (model.FingerprintResult, error) {
	result := model.FingerprintResult{URL: fileOrURL}

	switch ext := getExtension(fileOrURL); ext {
	case ".mpd":
		vs, err := m.fingerprintVariants(ctx, "dash", fileOrURL, baseURL)
		if err != nil {
			return model.FingerprintResult{}, err
		}
		result.Variants = &vs
	case ".m3u8":
		vs, err := m.fingerprintVariants(ctx, "hls", fileOrURL, baseURL)
		if err != nil {
			return model.FingerprintResult{}, err
		}
		result.Variants = &vs
	case ".mp4":
		v := model.Variant{
			MimeType:       "video/mp4",
			AddressingMode: "indexed",
			IndexedAddressingInfo: &model.IndexedAddressingInfo{
				URL:        fileOrURL,
				IndexRange: indexRange,
			},
		}
		fp, err := m.fingerprinters["default"].Fingerprint(ctx, v)
		if err != nil {
			return model.FingerprintResult{}, fmt.Errorf("fingerprint: %w", err)
		}
		result.Fingerprint = &fp
	default:
		return model.FingerprintResult{}, fmt.Errorf("unsupported file %q", ext)
	}

	return result, nil
}

func (m *Manager) extractVariants(ctx context.Context, service ID, reference model.Reference) ([]model.Variant, error) {
	ve, ok := m.variantExtractors[service]
	if !ok {
		return nil, fmt.Errorf("%q missing variant extractor", service)
	}

	return ve.ExtractVariants(ctx, reference)
}

func (m *Manager) fingerprintVariants(ctx context.Context, format, fileOrURL, baseURL string) ([]model.Variant, error) {
	ref := model.Reference{
		URL:     fileOrURL,
		Format:  format,
		Servers: []string{baseURL},
	}

	vs, err := m.variantExtractors["default"].ExtractVariants(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("extract variants: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	for i := range vs {
		g.Go(func() error {
			return m.fingerprint(ctx, "default", &vs[i])
		})
	}
	err = g.Wait()

	return vs, err
}

func (m *Manager) fingerprint(ctx context.Context, service ID, variant *model.Variant) error {
	f, ok := m.fingerprinters[service]
	if !ok {
		return fmt.Errorf("%q missing fingerprinter", service)
	}

	fp, err := f.Fingerprint(ctx, *variant)
	if err != nil {
		return err
	}
	variant.Fingerprint = &fp
	return nil
}

func getExtension(fileOrURL string) string {
	parsedURL, err := url.Parse(fileOrURL)
	if err != nil {
		return strings.ToLower(path.Ext(fileOrURL))
	}
	return strings.ToLower(path.Ext(parsedURL.Path))
}
