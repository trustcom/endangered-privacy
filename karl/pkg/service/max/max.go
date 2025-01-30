package max

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/net/html"
	"golang.org/x/sync/errgroup"
	"karl/pkg/config"
	"karl/pkg/model"
	"karl/pkg/service"
)

var (
	_ service.Client           = (*max)(nil)
	_ service.URLExtractor     = (*max)(nil)
	_ service.VideoExtractor   = (*max)(nil)
	_ service.VariantExtractor = (*max)(nil)
	_ service.Fingerprinter    = (*max)(nil)
)

type max struct {
	config            *config.AppConfig
	httpClient        *http.Client
	regex             *regexp.Regexp
	origin            string
	justWatchPackages []string
}

func New(config *config.AppConfig, httpClient *http.Client) service.Client {
	return &max{
		config:            config,
		httpClient:        httpClient,
		regex:             regexp.MustCompile(`max\.com/.*(movie|show|mini-series)s?/?.*/([a-z0-9\-]+)`),
		origin:            "https://play.max.com",
		justWatchPackages: []string{"mxx"},
	}
}

func (c *max) ID() service.ID {
	return "max"
}

func (c *max) ExtractURLs(ctx context.Context) ([]string, error) {
	var (
		urls []string
		mu   sync.Mutex
	)

	g, ctx := errgroup.WithContext(ctx)
	for _, mediaType := range []string{"movies", "shows"} {
		g.Go(func() error {
			u, err := c.extractURLs(ctx, mediaType)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				urls = append(urls, u...)
			}
			return err
		})
	}
	err := g.Wait()

	return urls, err
}

func (c *max) Matches(url string) bool {
	return c.regex.MatchString(url)
}

func (c *max) VideoExtract(ctx context.Context, url string) []model.VideoResult {
	var results []model.VideoResult

	for r := range c.extract(ctx, url) {
		results = append(results, r)
	}

	return results
}

func (c *max) ExtractVariants(ctx context.Context, reference model.Reference) ([]model.Variant, error) {
	return service.NewDefaultVariantExtractor(c.config, c.httpClient, c.origin).ExtractVariants(ctx, reference)
}

func (c *max) Fingerprint(ctx context.Context, variant model.Variant) (model.Fingerprint, error) {
	return service.NewDefaultFingerprinter(c.config, c.httpClient, c.origin).Fingerprint(ctx, variant)
}

func (c *max) fetchSiteMap(ctx context.Context, mediaType string) (io.ReadCloser, error) {
	u := fmt.Sprintf(
		"https://www.max.com/%s/en/sitemap/%s",
		strings.ToLower(c.config.CountryCode),
		mediaType,
	)

	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("new: %w", err)
		}

		res, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("do: %w", err)
		}

		if res.StatusCode != http.StatusOK {
			res.Body.Close()

			if res.StatusCode == http.StatusNotFound {
				u = "https://www.max.com/sitemap/" + mediaType
				continue
			}

			return nil, fmt.Errorf("status %s", res.Status)
		}

		return res.Body, nil
	}

	return nil, fmt.Errorf("status %d", http.StatusNotFound)
}

func (c *max) extractURLs(ctx context.Context, mediaType string) ([]string, error) {
	body, err := c.fetchSiteMap(ctx, mediaType)
	if err != nil {
		return nil, fmt.Errorf("fetch sitemap: %w", err)
	}
	defer body.Close()

	doc, err := html.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("html parse: %w", err)
	}

	var urls []string
	for ch := range doc.Descendants() {
		if ch.Type != html.ElementNode || ch.Data != "a" {
			continue
		}
		for _, attr := range ch.Attr {
			if attr.Key != "href" {
				continue
			}
			if u := "https://www.max.com" + attr.Val; c.regex.MatchString(u) {
				urls = append(urls, u)
			}
		}
	}

	return urls, nil
}

func (c *max) extract(ctx context.Context, url string) <-chan model.VideoResult {
	results := make(chan model.VideoResult)

	var (
		m         = c.regex.FindStringSubmatch(url)
		mediaType = m[1]
		id        = m[2]
	)

	go func() {
		defer close(results)

		switch mediaType {
		case "movie":
			c.sendMovie(ctx, id, results)
		case "show", "mini-series":
			c.sendSeries(ctx, id, results)
		default:
			results <- model.VideoResult{Err: fmt.Errorf("media type %q", mediaType)}
		}
	}()

	return results
}

func (c *max) sendMovie(ctx context.Context, id string, results chan<- model.VideoResult) {
	res, err := c.fetchMoviePage(ctx, id)
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("fetch movie page %q: %w", id, err)}
		return
	}

	m, err := res.movie()
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("movie %q: %w", id, err)}
		return
	}

	ref, duration, err := c.extractVideoReference(ctx, m.EditID)
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("extract reference %q: %w", id, err)}
		return
	}

	results <- model.VideoResult{
		Video: model.Video{
			ID:          m.ID,
			Title:       m.Name,
			PlaybackURL: "https://play.max.com/video/watch/" + m.ID + "/" + m.EditID,
			Duration:    duration,
		},
		References: []model.Reference{*ref},
	}
}

func (c *max) fetchCollection(ctx context.Context, resource, query string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		"https://default.any-any.prd.api.max.com/cms/collections/"+resource+query,
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

	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("status %s", res.Status)
	}

	return res.Body, nil
}

type (
	moviePageResponse struct {
		Data struct {
			Relationships struct {
				Items struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"items"`
			} `json:"relationships"`
		} `json:"data"`

		Included []struct {
			ID string `json:"id"`

			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`

			Relationships struct {
				ActiveVideoForShow struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"activeVideoForShow"`

				Edit struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"edit"`
			} `json:"relationships"`
		} `json:"included"`
	}

	movie struct {
		ID     string
		Name   string
		EditID string
	}
)

func (c *max) fetchMoviePage(ctx context.Context, id string) (*moviePageResponse, error) {
	query := "?include=default&ph%5Bshow.id%5D=" + id

	body, err := c.fetchCollection(ctx, "generic-movie-page-rail-hero", query)
	if err != nil {
		return nil, fmt.Errorf("fetch collection: %w", err)
	}
	defer body.Close()

	var r moviePageResponse
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

func (c *max) sendSeries(ctx context.Context, id string, results chan<- model.VideoResult) {
	res, err := c.fetchSeasonNumbers(ctx, id)
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("fetch season numbers %q: %w", id, err)}
		return
	}

	nums, err := res.numbers()
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("season numbers %q: %w", id, err)}
		return
	}

	var wg sync.WaitGroup
	for _, n := range nums {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.sendSeason(ctx, id, n, results)
		}()
	}
	wg.Wait()
}

type (
	seasonNumbersResponse struct {
		Data struct {
			Attributes struct {
				Component struct {
					Filters []struct {
						ID string `json:"id"`

						Options []struct {
							ID string `json:"id"`
						} `json:"options"`
					} `json:"filters"`
				} `json:"component"`
			} `json:"attributes"`
		} `json:"data"`
	}

	seasonPageResponse struct {
		Data struct {
			Relationships struct {
				Items struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"items"`
			} `json:"relationships"`
		} `json:"data"`

		Included []struct {
			ID string `json:"id"`

			Attributes struct {
				Name          string `json:"name"`
				SeasonNumber  int32  `json:"seasonNumber"`
				EpisodeNumber int32  `json:"episodeNumber"`
			} `json:"attributes"`

			Relationships struct {
				Video struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"video"`

				Show struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"show"`

				Edit struct {
					Data struct {
						ID string `json:"id"`
					} `json:"data"`
				} `json:"edit"`
			} `json:"relationships"`
		} `json:"included"`
	}

	episode struct {
		ID           string
		Name         string
		SeriesName   string
		Number       int32
		SeasonNumber int32
		EditID       string
	}
)

func (c *max) fetchSeasonNumbers(ctx context.Context, id string) (*seasonNumbersResponse, error) {
	query := "?include=items&pf%5BseasonNumber%5D&pf%5Bshow.id%5D=" + id

	body, err := c.fetchCollection(ctx, "generic-show-page-rail-episodes-tabbed-content", query)
	if err != nil {
		return nil, fmt.Errorf("fetch collection: %w", err)
	}
	defer body.Close()

	var r seasonNumbersResponse
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

func (c *max) sendSeason(ctx context.Context, id, num string, results chan<- model.VideoResult) {
	res, err := c.fetchSeason(ctx, id, num)
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("fetch season %q (%s): %w", id, num, err)}
		return
	}

	eps, err := res.episodes()
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("season %q (%s) episodes: %w", id, num, err)}
		return
	}

	var wg sync.WaitGroup
	for _, e := range eps {
		wg.Add(1)
		go func() {
			defer wg.Done()

			ref, duration, err := c.extractVideoReference(ctx, e.EditID)
			if err != nil {
				results <- model.VideoResult{
					Err: fmt.Errorf("extract reference %q (%s): %w", id, num, err),
				}
				return
			}

			results <- model.VideoResult{
				Video: model.Video{
					ID:          e.ID,
					Title:       model.OneTitle(e.SeriesName, e.Name, e.SeasonNumber, e.Number),
					PlaybackURL: "https://play.max.com/video/watch/" + e.ID + "/" + e.EditID,
					Duration:    duration,
				},
				References: []model.Reference{*ref},
			}
		}()
	}
	wg.Wait()
}

func (c *max) fetchSeason(ctx context.Context, id, number string) (*seasonPageResponse, error) {
	query := "?include=default&pf%5BseasonNumber%5D=" + number + "&pf%5Bshow.id%5D=" + id

	body, err := c.fetchCollection(ctx, "generic-show-page-rail-episodes-tabbed-content", query)
	if err != nil {
		return nil, fmt.Errorf("fetch collection: %w", err)
	}
	defer body.Close()

	var r seasonPageResponse
	if err := json.NewDecoder(body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

func (c *max) extractVideoReference(ctx context.Context, editID string) (*model.Reference, int32, error) {
	r, err := c.fetchPlaybackInfo(ctx, editID)
	if err != nil {
		return nil, 0, fmt.Errorf("fetch playback info %q: %w", editID, err)
	}

	var (
		id       string
		duration int32
	)

	for _, v := range r.Videos {
		if v.Type == "main" {
			id = v.ManifestationID
			duration = int32(v.Duration)
			break
		}
	}

	return &model.Reference{
		ID:     id,
		Format: r.Manifest.Format,
		URL:    r.Manifest.URL,
	}, duration, nil
}

type (
	playbackInfoResponse struct {
		Videos []struct {
			ManifestationID string  `json:"manifestationId"`
			Duration        float64 `json:"duration"`
			Type            string  `json:"type"`
		} `json:"videos"`

		Manifest struct {
			Format string `json:"format"`
			URL    string `json:"url"`
		} `json:"manifest"`
	}
)

func (c *max) fetchPlaybackInfo(ctx context.Context, editID string) (*playbackInfoResponse, error) {
	const fmtQuery = `{"editId": "%s", "appBundle": "", "consumptionType": "streaming",
		"deviceInfo": {"player": {"sdk": {"name": "", "version": ""}, "mediaEngine": {
		"name": "", "version": ""}, "playerView": {"height": 2160, "width": 3840}}},
		"capabilities": {"manifests": {"formats": {"dash": {}}}, "codecs": {"audio": {
		"decoders": [{"codec": "avc", "profiles": ["lc", "hev", "hev2"]}]}, "video": {
		"decoders": [{"codec": "h264", "profiles": ["high", "main", "baseline"],
		"maxLevel": "5.2", "levelConstraints": {"width": {"min": 0, "max": 3840},
		"height": {"min": 0, "max": 2160}, "framerate": {"min": 0, "max": 60}}}],
		"hdrFormats": []}}}, "gdpr": false, "firstPlay": false, "playbackSessionId": "",
		"applicationSessionId": "", "userPreferences": { "videoQuality": "best"}}`

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://default.any-any.prd.api.max.com/any/playback/v1/playbackInfo",
		strings.NewReader(fmt.Sprintf(fmtQuery, editID)),
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

	var r playbackInfoResponse
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

func (r *moviePageResponse) movie() (movie, error) {
	videoID := ""
	for _, it := range r.Data.Relationships.Items.Data {
		for _, inc := range r.Included {
			if inc.ID == it.ID {
				videoID = inc.Relationships.ActiveVideoForShow.Data.ID
				break
			}
		}
		if videoID != "" {
			break
		}
	}
	for _, inc := range r.Included {
		if inc.ID == videoID {
			return movie{
				ID:     videoID,
				Name:   inc.Attributes.Name,
				EditID: inc.Relationships.Edit.Data.ID,
			}, nil
		}
	}

	return movie{}, errors.New("not found")
}

func (r *seasonNumbersResponse) numbers() ([]string, error) {
	var nums []string
	for _, f := range r.Data.Attributes.Component.Filters {
		if f.ID == "seasonNumber" {
			for _, o := range f.Options {
				nums = append(nums, o.ID)
			}
		}
	}
	if len(nums) == 0 {
		return nil, errors.New("not found")
	}

	return nums, nil
}

func (r *seasonPageResponse) episodes() ([]episode, error) {
	var (
		videoIDs []string
		episodes []episode
	)
	for _, it := range r.Data.Relationships.Items.Data {
		for _, inc := range r.Included {
			if inc.ID == it.ID {
				videoIDs = append(videoIDs, inc.Relationships.Video.Data.ID)
			}
		}
	}

	seriesName := ""
	for _, inc := range r.Included {
		if !slices.Contains(videoIDs, inc.ID) {
			continue
		}
		if seriesName == "" {
			for _, incl := range r.Included {
				if incl.ID == inc.Relationships.Show.Data.ID {
					seriesName = incl.Attributes.Name
					break
				}
			}
		}
		episodes = append(episodes, episode{
			ID:           inc.ID,
			Name:         inc.Attributes.Name,
			SeriesName:   seriesName,
			Number:       inc.Attributes.EpisodeNumber,
			SeasonNumber: inc.Attributes.SeasonNumber,
			EditID:       inc.Relationships.Edit.Data.ID,
		})
	}
	if len(episodes) == 0 {
		return nil, errors.New("not found")
	}

	return episodes, nil
}
