package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/abema/go-mp4"
	"golang.org/x/sync/errgroup"
	"karl/pkg/config"
	"karl/pkg/model"
)

var _ Fingerprinter = (*DefaultFingerprinter)(nil)

type DefaultFingerprinter struct {
	config     *config.AppConfig
	httpClient *http.Client
	origin     string
}

func NewDefaultFingerprinter(config *config.AppConfig, httpClient *http.Client, origin string) *DefaultFingerprinter {
	return &DefaultFingerprinter{
		config:     config,
		httpClient: httpClient,
		origin:     origin,
	}
}

func (f *DefaultFingerprinter) Fingerprint(ctx context.Context, variant model.Variant) (model.Fingerprint, error) {
	switch m := variant.AddressingMode; m {
	case "indexed":
		return f.fingerprintIndexed(ctx, variant.MimeType, *variant.IndexedAddressingInfo)
	case "explicit":
		return f.fingerprintExplicit(ctx, *variant.ExplicitAddressingInfo)
	case "fingerprinted":
		return *variant.Fingerprint, nil
	default:
		return model.Fingerprint{}, fmt.Errorf("unsupported addressing mode %q", m)
	}
}

func (f *DefaultFingerprinter) fingerprintIndexed(ctx context.Context, mimeType string, info model.IndexedAddressingInfo) (model.Fingerprint, error) {
	switch mimeType {
	case "video/mp4":
		return f.fingerprintIndexedMP4(ctx, info)
	case "video/webm":
		return model.Fingerprint{}, errors.New("webm not yet implemented")
	default:
		return model.Fingerprint{}, fmt.Errorf("unsupported mime type %q", mimeType)
	}
}

func (f *DefaultFingerprinter) fingerprintIndexedMP4(ctx context.Context, info model.IndexedAddressingInfo) (model.Fingerprint, error) {
	parsed, err := url.ParseRequestURI(info.URL)
	var (
		raw        []byte
		indexRange = info.IndexRange
		isURL      = err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https")
	)
	if indexRange == "" {
		indexRange = "0-65535"
	}
	if isURL {
		raw, err = f.fetchIndex(ctx, info.URL, indexRange)
		if err != nil {
			return model.Fingerprint{}, fmt.Errorf("fetch index: %w", err)
		}
	} else {
		raw, err = readRange(info.URL, indexRange)
		if err != nil {
			return model.Fingerprint{}, fmt.Errorf("read file: %w", err)
		}
	}

	sidx, err := f.extractSIDX(raw)
	if err != nil {
		return model.Fingerprint{}, fmt.Errorf("extract sidx: %w", err)
	}

	fp := model.Fingerprint{
		SegmentSizes:     make([]uint32, len(sidx.References)),
		SegmentDurations: make([]uint32, len(sidx.References)),
		Timescale:        sidx.Timescale,
	}

	for i, r := range sidx.References {
		fp.SegmentSizes[i] = r.ReferencedSize
		fp.SegmentDurations[i] = r.SubsegmentDuration
	}

	return fp, nil
}

func (f *DefaultFingerprinter) fetchIndex(ctx context.Context, url, indexRange string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}

	if f.origin != "" {
		req.Header.Set("Origin", f.origin)
		req.Header.Set("Referer", f.origin+"/")
	}

	req.Header.Set("Range", "bytes="+indexRange)

	res, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	return io.ReadAll(res.Body)
}

func (f *DefaultFingerprinter) extractSIDX(raw []byte) (*mp4.Sidx, error) {
	boxes, err := mp4.ExtractBoxWithPayload(
		bytes.NewReader(raw),
		nil,
		mp4.BoxPath{mp4.BoxTypeSidx()},
	)
	if err != nil {
		return nil, err
	}

	if len(boxes) > 0 {
		if sidx, ok := boxes[0].Payload.(*mp4.Sidx); ok {
			return sidx, nil
		}
	}

	return nil, errors.New("sidx box not found")
}

func (f *DefaultFingerprinter) fingerprintExplicit(ctx context.Context, info model.ExplicitAddressingInfo) (model.Fingerprint, error) {
	fp := model.Fingerprint{
		SegmentSizes:     make([]uint32, len(info.URLs)),
		SegmentDurations: info.SegmentDurations,
		Timescale:        info.Timescale,
	}

	g, ctx := errgroup.WithContext(ctx)
	for i, u := range info.URLs {
		g.Go(func() error {
			const (
				retries    = 5
				maxSleepMS = 1000
			)
			try := 0
			for {
				timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				if l := len(info.Servers); l > 0 {
					u = strings.Replace(u, "$Server$", info.Servers[rand.Intn(l)], 1)
				}
				l, err := f.fetchContentLength(timeoutCtx, u)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil && try < retries {
					cancel()
					time.Sleep(time.Duration(rand.Intn(maxSleepMS)) * time.Millisecond)
					try++
					continue
				}
				if err != nil {
					return fmt.Errorf("fetch content length: %w", err)
				}
				if l > math.MaxUint32 {
					return errors.New("content length > uint32")
				}
				fp.SegmentSizes[i] = uint32(l)
				return nil
			}
		})
	}
	err := g.Wait()

	return fp, err
}

func (f *DefaultFingerprinter) fetchContentLength(ctx context.Context, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, fmt.Errorf("new: %w", err)
	}

	if f.origin != "" {
		req.Header.Set("Origin", f.origin)
		req.Header.Set("Referer", f.origin+"/")
	}

	res, err := f.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	return res.ContentLength, nil
}

func readRange(filename string, indexRange string) ([]byte, error) {
	startStr, endStr, _ := strings.Cut(indexRange, "-")
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return nil, err
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	buf := make([]byte, end-start+1)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}

	return buf, nil
}
