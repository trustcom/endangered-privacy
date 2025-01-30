package svt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"karl/pkg/config"
	"karl/pkg/model"
	"karl/pkg/service"
)

var (
	_ service.Client           = (*svt)(nil)
	_ service.URLExtractor     = (*svt)(nil)
	_ service.VideoExtractor   = (*svt)(nil)
	_ service.VariantExtractor = (*svt)(nil)
	_ service.Fingerprinter    = (*svt)(nil)
)

type svt struct {
	config     *config.AppConfig
	httpClient *http.Client
	regex      *regexp.Regexp
	origin     string
}

func New(config *config.AppConfig, httpClient *http.Client) service.Client {
	return &svt{
		config:     config,
		httpClient: httpClient,
		regex:      regexp.MustCompile(`svtplay.se/(video/\w+|[\w-]+)`),
		origin:     "https://www.svtplay.se",
	}
}

func (c *svt) ID() service.ID {
	return "svt"
}

func (c *svt) ExtractURLs(ctx context.Context) ([]string, error) {
	return c.extractURLs(ctx)
}

func (c *svt) Matches(url string) bool {
	return c.regex.MatchString(url)
}

func (c *svt) VideoExtract(ctx context.Context, url string) []model.VideoResult {
	var results []model.VideoResult

	for r := range c.extract(ctx, url) {
		results = append(results, r)
	}

	return results
}

func (c *svt) ExtractVariants(ctx context.Context, reference model.Reference) ([]model.Variant, error) {
	return service.NewDefaultVariantExtractor(c.config, c.httpClient, c.origin).ExtractVariants(ctx, reference)
}

func (c *svt) Fingerprint(ctx context.Context, variant model.Variant) (model.Fingerprint, error) {
	return service.NewDefaultFingerprinter(c.config, c.httpClient, c.origin).Fingerprint(ctx, variant)
}

func (c *svt) extractURLs(ctx context.Context) ([]string, error) {
	res, err := c.fetchGraphQLURLs(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch urls: %w", err)
	}
	if len(res.Errors) > 0 {
		return nil, res.Errors[0]
	}

	return res.Data.urls(c.config.CountryCode), nil
}

func (c *svt) fetchGraphQLURLs(ctx context.Context) (*graphQLURLResponse, error) {
	const query = `{"query": ` +
		`"query { programAtillO(filter: {includeFullOppetArkiv: true}) ` +
		`{ flat { episodes { urls { svtplay } hasVideoReferences ` +
		`restrictions { onlyAvailableInSweden } } } } }"}`

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://api.svt.se/contento/graphql",
		strings.NewReader(query),
	)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Referer", c.origin+"/")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", res.Status)
	}

	var r graphQLURLResponse
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

type (
	graphQLURLResponse struct {
		Data   graphQLURLData `json:"data"`
		Errors []graphQLError `json:"errors"`
	}

	graphQLURLData struct {
		ProgramAtillO struct {
			Flat []struct {
				Episodes []struct {
					URLs struct {
						SvtPlay string `json:"svtplay"`
					} `json:"urls"`

					HasVideoReferences bool `json:"hasVideoReferences"`

					Restrictions struct {
						OnlyAvailableInSweden bool `json:"onlyAvailableInSweden"`
					} `json:"restrictions"`
				} `json:"episodes"`
			} `json:"flat"`
		} `json:"programAtillO"`
	}

	graphQLError struct {
		Extensions struct {
			Classification string `json:"classification"`
		} `json:"extensions"`
	}
)

func (d *graphQLURLData) urls(country string) []string {
	paths := make(map[string]struct{})
	for _, p := range d.ProgramAtillO.Flat {
		for _, e := range p.Episodes {
			geoBlocked := country != "SE" && e.Restrictions.OnlyAvailableInSweden
			if e.URLs.SvtPlay != "" && e.HasVideoReferences && !geoBlocked {
				paths[e.URLs.SvtPlay] = struct{}{}
			}
		}
	}

	urls := make([]string, 0, len(paths))
	for path := range paths {
		urls = append(urls, "https://www.svtplay.se"+path)
	}

	return urls
}

func (e graphQLError) Error() string {
	return "graphql: " + e.Extensions.Classification
}

func (c *svt) extract(ctx context.Context, url string) <-chan model.VideoResult {
	results := make(chan model.VideoResult)

	var (
		match     = c.regex.FindStringSubmatch(url)
		id, found = strings.CutPrefix(match[1], "video/")
		ids       = []string{id}
	)

	go func() {
		defer close(results)

		if !found {
			var (
				path = match[1]
				err  error
			)

			ids, err = c.extractPathIDs(ctx, path)
			if err != nil {
				results <- model.VideoResult{Err: err}
				return
			}
		}

		c.sendVideos(ctx, ids, results)
	}()

	return results
}

func (c *svt) extractPathIDs(ctx context.Context, path string) ([]string, error) {
	res, err := c.fetchGraphQLPathIDs(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("fetch path ids %q: %w", path, err)
	}
	if len(res.Errors) > 0 {
		return nil, res.Errors[0]
	}

	ids := res.Data.pathIDs()
	if len(ids) == 0 {
		return nil, fmt.Errorf("no ids for %q", path)
	}

	return ids, nil
}

func (c *svt) fetchGraphQLPathIDs(ctx context.Context, path string) (*graphQLPathIDsResponse, error) {
	const fmtQuery = `{"query": ` +
		`"query { detailsPageByPath(path: \"/%s\", filter: {includeFullOppetArkiv: true}) ` +
		`{ video { svtId } associatedContent(include: [productionPeriod, season]) ` +
		`{ items(filter: {includeFullOppetArkiv: true}) { item { videoSvtId } } } } }"}`

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://api.svt.se/contento/graphql",
		strings.NewReader(fmt.Sprintf(fmtQuery, path)),
	)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Referer", c.origin+"/")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", res.Status)
	}

	var r graphQLPathIDsResponse
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

type (
	graphQLPathIDsResponse struct {
		Data   graphQLPathIDsData `json:"data"`
		Errors []graphQLError     `json:"errors"`
	}

	graphQLPathIDsData struct {
		DetailsPageByPath struct {
			Video struct {
				SvtID string `json:"svtId"`
			} `json:"video"`

			AssociatedContent []struct {
				Items []struct {
					Item struct {
						VideoSvtID string `json:"videoSvtId"`
					} `json:"item"`
				} `json:"items"`
			} `json:"associatedContent"`
		} `json:"detailsPageByPath"`
	}
)

func (d *graphQLPathIDsData) pathIDs() []string {
	idSet := make(map[string]struct{})

	if d.DetailsPageByPath.Video.SvtID != "" {
		idSet[d.DetailsPageByPath.Video.SvtID] = struct{}{}
	}

	for _, ac := range d.DetailsPageByPath.AssociatedContent {
		for _, i := range ac.Items {
			if i.Item.VideoSvtID != "" {
				idSet[i.Item.VideoSvtID] = struct{}{}
			}
		}
	}

	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}

	return ids
}

func (c *svt) sendVideos(ctx context.Context, ids []string, results chan<- model.VideoResult) {
	var wg sync.WaitGroup
	for _, id := range ids[1:] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.sendVideo(ctx, id, results)
		}()
	}
	c.sendVideo(ctx, ids[0], results)
	wg.Wait()
}

func (c *svt) sendVideo(ctx context.Context, id string, results chan<- model.VideoResult) {
	res, err := c.fetchVideo(ctx, id)
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("fetch video %q: %w", id, err)}
		return
	}

	results <- model.VideoResult{Video: res.video(), References: res.references()}
}

func (c *svt) fetchVideo(ctx context.Context, id string) (*videoResponse, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		"https://video.svt.se/video/"+id,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}

	req.Header.Set("Origin", c.origin)
	req.Header.Set("Referer", c.origin+"/")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", res.Status)
	}

	var r videoResponse
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

type videoResponse struct {
	SvtID           string `json:"svtId"`
	ProgramTitle    string `json:"programTitle"`
	EpisodeTitle    string `json:"episodeTitle"`
	ContentDuration int32  `json:"contentDuration"`

	Rights struct {
		ValidTo time.Time `json:"validTo"`
	} `json:"rights"`

	VideoReferences []struct {
		URL    string `json:"url"`
		Format string `json:"format"`
	} `json:"videoReferences"`
}

func (r *videoResponse) video() model.Video {
	return model.Video{
		ID:          r.SvtID,
		Title:       model.OneTitle(r.ProgramTitle, r.EpisodeTitle, 0, 0),
		PlaybackURL: "https://www.svtplay.se/video/" + r.SvtID,
		Duration:    r.ContentDuration,
		ExpiresAt:   &r.Rights.ValidTo,
	}
}

var (
	akamaiRe = regexp.MustCompile(`[a-zA-Z]\.akamaized\.net`)
	servers  = []string{"a", "b", "c"}
)

func (r *videoResponse) references() []model.Reference {
	refs := make([]model.Reference, len(r.VideoReferences))
	for i, ref := range r.VideoReferences {
		format := ""
		switch {
		case strings.HasPrefix(ref.Format, "dash"):
			format = "dash"
		case strings.HasPrefix(ref.Format, "hls"):
			format = "hls"
		default:
			continue
		}
		refs[i] = model.Reference{
			ID:      ref.Format,
			Format:  format,
			URL:     akamaiRe.ReplaceAllString(ref.URL, "$$Server$$.akamaized.net"),
			Servers: servers,
		}
	}

	return refs
}
