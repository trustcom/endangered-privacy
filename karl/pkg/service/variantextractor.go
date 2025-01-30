package service

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Eyevinn/dash-mpd/mpd"
	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"golang.org/x/sync/errgroup"
	"karl/pkg/config"
	"karl/pkg/model"
)

var _ VariantExtractor = (*DefaultVariantExtractor)(nil)

type DefaultVariantExtractor struct {
	config     *config.AppConfig
	httpClient *http.Client
	origin     string
}

func NewDefaultVariantExtractor(config *config.AppConfig, httpClient *http.Client, origin string) *DefaultVariantExtractor {
	return &DefaultVariantExtractor{
		config:     config,
		httpClient: httpClient,
		origin:     origin,
	}
}

func (ve *DefaultVariantExtractor) ExtractVariants(ctx context.Context, reference model.Reference) ([]model.Variant, error) {
	switch f := reference.Format; f {
	case "dash":
		return ve.extractMPDVariants(ctx, reference)
	case "hls":
		return ve.extractM3U8Variants(ctx, reference)
	default:
		return nil, fmt.Errorf("unsupported format %q", f)
	}
}

func (ve *DefaultVariantExtractor) extractMPDVariants(ctx context.Context, reference model.Reference) ([]model.Variant, error) {
	parsed, err := url.ParseRequestURI(reference.URL)
	var (
		m     *mpd.MPD
		u     = reference.URL
		isURL = err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
	)
	if isURL {
		if l := len(reference.Servers); l > 0 {
			u = strings.Replace(u, "$Server$", reference.Servers[rand.Intn(l)], 1)
		}
		m, err = ve.fetchMPD(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("fetch mpd: %w", err)
		}
	} else {
		m, err = mpd.ReadFromFile(u)
		if err != nil {
			return nil, fmt.Errorf("read mpd: %w", err)
		}
		if len(reference.Servers) > 0 {
			u = reference.Servers[0]
		}
		if u == "" && len(m.BaseURL) > 0 {
			u = string(m.BaseURL[0].Value)
		}
	}

	if m.GetType() != mpd.STATIC_TYPE {
		return nil, errors.New("mpd is not static")
	}

	u = resolveBaseURLTypes(u, m.BaseURL)
	group := newVariantGroup()
	for _, p := range m.Periods {
		var periodDuration time.Duration
		if d, err := p.GetDuration(); err == nil {
			periodDuration = time.Duration(d)
		}

		ad := false
		for _, prop := range p.SupplementalProperties {
			if prop != nil && strings.ToLower(prop.Value) == "ad" {
				ad = true
				break
			}
		}
		if ad {
			continue
		}

		u := resolveBaseURLTypes(u, p.BaseURLs)
		for _, as := range p.AdaptationSets {
			if as.ContentType != "" && as.ContentType != "video" {
				continue
			}

			u := resolveBaseURLTypes(u, as.BaseURLs)
			for _, r := range as.Representations {
				if m := r.GetMimeType(); m != "" && !strings.HasPrefix(m, "video") {
					continue
				}

				u := resolveBaseURLTypes(u, r.BaseURLs)
				v, err := ve.extractMPDVariant(u, reference.Servers, r)
				if err != nil {
					return nil, fmt.Errorf("extract mpd variant: %w", err)
				}

				group.add(v, periodDuration)
			}
		}
	}
	if v := group.merge(); len(v) > 0 {
		return v, nil
	}

	return nil, errors.New("no variants found")
}

func (ve *DefaultVariantExtractor) fetchMPD(ctx context.Context, url string) (*mpd.MPD, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}

	if ve.origin != "" {
		req.Header.Set("Origin", ve.origin)
		req.Header.Set("Referer", ve.origin+"/")
	}

	res, err := ve.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return mpd.MPDFromBytes(raw)
}

func (ve *DefaultVariantExtractor) extractMPDVariant(u string, servers []string, r *mpd.RepresentationType) (*model.Variant, error) {
	var (
		mimeType = r.GetMimeType()
		codecs   = r.GetCodecs()
	)

	v := &model.Variant{
		ID:        computeID(mimeType, codecs, r.Width, r.Height, r.Bandwidth),
		MimeType:  mimeType,
		Codecs:    codecs,
		Width:     r.Width,
		Height:    r.Height,
		Bandwidth: r.Bandwidth,
	}

	switch {
	case r.SegmentBase != nil:
		v.AddressingMode = "indexed"
		if len(servers) > 0 {
			u = strings.Replace(u, "$Server$", servers[rand.Intn(len(servers))], 1)
		}
		v.IndexedAddressingInfo = &model.IndexedAddressingInfo{
			URL:        u,
			IndexRange: r.SegmentBase.IndexRange,
		}
	case r.SegmentTemplate != nil:
		v.AddressingMode = "explicit"
		info, err := parseMPDExplicitAddressingInfo(u, r.SegmentTemplate)
		if err != nil {
			return nil, fmt.Errorf("explicit addressing info: %w", err)
		}
		info.Servers = servers
		v.ExplicitAddressingInfo = info
	case r.SegmentList != nil:
		return nil, errors.New("segment list not implemented")
	default:
		return nil, errors.New("unknown addressing type")
	}

	return v, nil
}

func parseMPDExplicitAddressingInfo(u string, st *mpd.SegmentTemplateType) (*model.ExplicitAddressingInfo, error) {
	if st.SegmentTimeline == nil {
		return nil, errors.New("missing segment timeline")
	}

	info := &model.ExplicitAddressingInfo{
		TemplateURL: resolveReference(u, st.Media),
		Timescale:   st.GetTimescale(),
	}

	timePlaceholder := false
	if strings.Contains(st.Media, "$Time$") {
		timePlaceholder = true
	}
	if !timePlaceholder && !strings.Contains(st.Media, "$Number$") {
		return nil, fmt.Errorf("unknown placeholder in %q", st.Media)
	}

	num := 1
	if st.StartNumber != nil {
		num = int(*st.StartNumber)
	}

	for _, s := range st.SegmentTimeline.S {
		if s == nil {
			continue
		}

		if s.D > math.MaxUint32 {
			return nil, errors.New("segment duration > uint32")
		}

		if timePlaceholder {
			if s.T == nil {
				return nil, errors.New("missing time in segment timeline")
			}
			info.URLs = append(
				info.URLs,
				strings.Replace(info.TemplateURL, "$Time$", strconv.FormatUint(*s.T, 10), 1),
			)
			info.SegmentDurations = append(info.SegmentDurations, uint32(s.D))
			continue
		}

		if s.R < 0 {
			return nil, errors.New("unlimited repeat in segment timeline")
		}
		for range 1 + s.R {
			info.URLs = append(
				info.URLs,
				strings.Replace(info.TemplateURL, "$Number$", strconv.Itoa(num), 1),
			)
			info.SegmentDurations = append(info.SegmentDurations, uint32(s.D))
			num++
		}
	}

	return info, nil
}

func (ve *DefaultVariantExtractor) extractM3U8Variants(ctx context.Context, reference model.Reference) ([]model.Variant, error) {
	parsed, err := url.ParseRequestURI(reference.URL)
	var (
		p     playlist.Playlist
		u     = reference.URL
		isURL = err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
	)
	if isURL {
		if l := len(reference.Servers); l > 0 {
			u = strings.Replace(u, "$Server$", reference.Servers[rand.Intn(l)], 1)
		}
		p, err = ve.fetchM3U8(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("fetch m3u8: %w", err)
		}
	} else {
		b, err := os.ReadFile(u)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		p, err = playlist.Unmarshal(b)
		if err != nil {
			return nil, fmt.Errorf("read m3u8: %w", err)
		}
		if len(reference.Servers) > 0 {
			u = reference.Servers[0]
		}
	}

	g, ctx := errgroup.WithContext(ctx)
	if p, ok := p.(*playlist.Multivariant); ok {
		variants := make([]model.Variant, len(p.Variants))
		for i, v := range p.Variants {
			if v.Resolution == "" {
				continue
			}
			g.Go(func() error {
				variant, err := ve.extractM3U8Variant(ctx, u, reference.Servers, v)
				if err != nil {
					return fmt.Errorf("extract m3u8 variant: %w", err)
				}
				variants[i] = *variant
				return nil
			})
		}
		err := g.Wait()
		var filtered []model.Variant
		for _, v := range variants {
			if v.AddressingMode == "" {
				continue
			}
			filtered = append(filtered, v)
		}
		return filtered, err
	}

	return nil, errors.New("master playlist not found")
}

func (ve *DefaultVariantExtractor) fetchM3U8(ctx context.Context, url string) (playlist.Playlist, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}

	if ve.origin != "" {
		req.Header.Set("Origin", ve.origin)
		req.Header.Set("Referer", ve.origin+"/")
	}

	res, err := ve.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	return playlist.Unmarshal(raw)
}

func (ve *DefaultVariantExtractor) extractM3U8Variant(ctx context.Context, url string, servers []string, v *playlist.MultivariantVariant) (*model.Variant, error) {
	widthStr, heightStr, ok := strings.Cut(v.Resolution, "x")
	if !ok {
		return nil, fmt.Errorf("resolution: %s", v.Resolution)
	}

	width, err := strconv.ParseUint(widthStr, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("width: %w", err)
	}

	height, err := strconv.ParseUint(heightStr, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("height: %w", err)
	}

	if v.Bandwidth > math.MaxUint32 {
		return nil, errors.New("bandwidth > uint32")
	}
	bandwidth := uint32(v.Bandwidth)

	if len(v.Codecs) == 0 {
		return nil, errors.New("no codecs")
	}
	codecs := v.Codecs[0]

	u := resolveReference(url, v.URI)
	p, err := ve.fetchM3U8(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("fetch m3u8: %w", err)
	}

	variant := &model.Variant{
		Codecs:    codecs,
		Width:     uint32(width),
		Height:    uint32(height),
		Bandwidth: bandwidth,
	}

	var (
		fp        model.Fingerprint
		isIndexed bool
	)
	info := &model.ExplicitAddressingInfo{
		Servers:   servers,
		Timescale: 1000,
	}

	if p, ok := p.(*playlist.Media); ok {
		for _, seg := range p.Segments {
			if variant.MimeType == "" {
				switch filepath.Ext(seg.URI) {
				case ".ts":
					variant.MimeType = "video/mp2t"
				case ".m4s", ".m4v", ".mp4":
					variant.MimeType = "video/mp4"
				}
			}

			dur := seg.Duration.Milliseconds()
			if dur > math.MaxUint32 {
				return nil, errors.New("segment duration > uint32")
			}

			if seg.ByteRangeLength != nil {
				if !isIndexed {
					variant.AddressingMode = "fingerprinted"
					variant.Fingerprint = &fp
					isIndexed = true
					fp.Timescale = 1000
				}
				size := *seg.ByteRangeLength
				if size > math.MaxUint32 {
					return nil, errors.New("segment size > uint32")
				}
				fp.SegmentSizes = append(variant.Fingerprint.SegmentSizes, uint32(size))
				fp.SegmentDurations = append(variant.Fingerprint.SegmentDurations, uint32(dur))
				continue
			}

			info.URLs = append(info.URLs, resolveReference(u, seg.URI))
			info.SegmentDurations = append(info.SegmentDurations, uint32(dur))
		}

		variant.ID = computeID(variant.MimeType, variant.Codecs, variant.Width, variant.Height, variant.Bandwidth)

		if !isIndexed {
			variant.AddressingMode = "explicit"
			variant.ExplicitAddressingInfo = info
		}

		return variant, nil
	}

	return nil, errors.New("media playlist not found")
}

type variantGroup struct {
	variants    map[string][]*model.Variant
	durations   map[string]time.Duration
	maxDuration time.Duration
}

func newVariantGroup() *variantGroup {
	return &variantGroup{
		variants:  make(map[string][]*model.Variant),
		durations: make(map[string]time.Duration),
	}
}

func (vg *variantGroup) add(v *model.Variant, d time.Duration) {
	k := ""
	switch v.AddressingMode {
	case "indexed":
		k = v.IndexedAddressingInfo.URL
	case "explicit":
		k = v.ExplicitAddressingInfo.TemplateURL
	}
	vg.variants[k] = append(vg.variants[k], v)
	vg.durations[k] += d
	vg.maxDuration = max(vg.maxDuration, vg.durations[k])
}

// merge merges multi-period variants, averaging bandwidths
// and possibly extending timelines.
func (vg *variantGroup) merge() []model.Variant {
	var merged []model.Variant
	for k, vs := range vg.variants {
		// Skip variants that are not present for as long
		// as the longest variant(s). Likely not part of
		// the main content (e.g. ads).
		if vg.durations[k] < vg.maxDuration {
			continue
		}

		var (
			m   = *vs[0]
			sum = int64(m.Bandwidth)
		)

		for _, v := range vs[1:] {
			sum += int64(v.Bandwidth)
			if m.AddressingMode == "explicit" {
				var (
					urls = &m.ExplicitAddressingInfo.URLs
					durs = &m.ExplicitAddressingInfo.SegmentDurations
				)
				*urls = append(*urls, v.ExplicitAddressingInfo.URLs...)
				*durs = append(*durs, v.ExplicitAddressingInfo.SegmentDurations...)
			}
		}

		m.Bandwidth = uint32(sum / int64(len(vs)))
		if m.Bandwidth != vs[0].Bandwidth {
			m.ID = computeID(m.MimeType, m.Codecs, m.Width, m.Height, m.Bandwidth)
		}

		merged = append(merged, m)
	}

	return merged
}

func resolveBaseURLTypes(baseURL string, uTypes []*mpd.BaseURLType) string {
	if len(uTypes) == 0 || uTypes[0] == nil {
		return baseURL
	}
	return resolveReference(baseURL, string(uTypes[0].Value))
}

func resolveReference(baseURL, u string) string {
	ref, err := url.Parse(u)
	if err != nil {
		return baseURL
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	return base.ResolveReference(ref).String()
}

func computeID(mimeType, codecs string, width, height, bandwidth uint32) string {
	hash := md5.Sum([]byte(fmt.Sprintf("%s-%s-%d-%d-%d", mimeType, codecs, width, height, bandwidth)))
	return hex.EncodeToString(hash[:])
}
