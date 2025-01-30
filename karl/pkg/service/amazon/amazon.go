package amazon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	"karl/pkg/config"
	"karl/pkg/model"
	"karl/pkg/service"
)

var (
	_ service.Client           = (*amazon)(nil)
	_ service.URLExtractor     = (*amazon)(nil)
	_ service.VideoExtractor   = (*amazon)(nil)
	_ service.VariantExtractor = (*amazon)(nil)
	_ service.Fingerprinter    = (*amazon)(nil)
)

type amazon struct {
	config            *config.AppConfig
	httpClient        *http.Client
	regex             *regexp.Regexp
	origin            string
	justWatchPackages []string
}

func New(config *config.AppConfig, httpClient *http.Client) service.Client {
	return &amazon{
		config:     config,
		httpClient: httpClient,
		regex: regexp.MustCompile(
			`((?:amazon|primevideo)\.[^/]+).*(?:(?:(?:gti|asin|creativeASIN)=|(?:detail|dp)/)([\w\.\-]+))`,
		),
		origin:            "https://www.primevideo.com",
		justWatchPackages: []string{"amp", "prv"},
	}
}

func (c *amazon) ID() service.ID {
	return "amazon"
}

func (c *amazon) ExtractURLs(ctx context.Context) ([]string, error) {
	return service.NewJustWatchURLExtractor(c.config, c.httpClient, c.justWatchPackages).ExtractURLs(ctx)
}

func (c *amazon) Matches(url string) bool {
	return c.regex.MatchString(url)
}

func (c *amazon) VideoExtract(ctx context.Context, url string) []model.VideoResult {
	var results []model.VideoResult

	for r := range c.extract(ctx, url) {
		results = append(results, r)
	}

	return results
}

func (c *amazon) ExtractVariants(ctx context.Context, reference model.Reference) ([]model.Variant, error) {
	return service.NewDefaultVariantExtractor(c.config, c.httpClient, c.origin).ExtractVariants(ctx, reference)
}

func (c *amazon) Fingerprint(ctx context.Context, variant model.Variant) (model.Fingerprint, error) {
	return service.NewDefaultFingerprinter(c.config, c.httpClient, c.origin).Fingerprint(ctx, variant)
}

func (c *amazon) extract(ctx context.Context, url string) <-chan model.VideoResult {
	results := make(chan model.VideoResult)

	var (
		m      = c.regex.FindStringSubmatch(url)
		domain = m[1]
		id     = m[2]
	)

	go func() {
		defer close(results)

		w, err := c.extractDetailPageWidgets(ctx, domain, id)
		if err != nil {
			results <- model.VideoResult{Err: err}
			return
		}

		switch t := w.PageContext.SubPageType; t {
		case "Movie":
			c.sendMovie(ctx, domain, id, w.movie(), results)
		case "Season":
			c.sendSeries(ctx, domain, id, w.season(), results)
		default:
			results <- model.VideoResult{Err: fmt.Errorf("page type %q", t)}
		}
	}()

	return results
}

type (
	detailPageResponse struct {
		Widgets detailPageWidgets `json:"widgets"`
	}

	detailPageWidgets struct {
		PageContext struct {
			SubPageType string `json:"subPageType"`
		} `json:"pageContext"`

		Self detailPageSelf `json:"self"`

		Header struct {
			Detail detailPageDetail `json:"detail"`
		} `json:"header"`

		BuyBox struct {
			Action detailPageAction `json:"action"`
		} `json:"buybox"`

		SeasonSelector []struct {
			TitleID    string `json:"titleID"`
			IsSelected bool   `json:"isSelected"`
		} `json:"seasonSelector"`

		EpisodeList struct {
			Actions struct {
				Pagination []detailPagePagination `json:"pagination"`
			} `json:"actions"`

			Episodes []struct {
				Self   detailPageSelf   `json:"self"`
				Detail detailPageDetail `json:"detail"`
			} `json:"episodes"`
		} `json:"episodeList"`
	}

	detailPageAction struct {
		AcquisitionActions struct {
			PrimaryWaysToWatch []struct {
				Children []struct {
					SType string `json:"sType"`
				} `json:"children"`
			} `json:"primaryWaysToWatch"`

			MoreWaysToWatch struct {
				Children []struct {
					SType string `json:"sType"`
				} `json:"children"`
			} `json:"moreWaysToWatch"`
		} `json:"acquisitionActions"`

		PlaybackActions struct {
			Main struct {
				Children []struct {
					BenefitID string `json:"benefitId"`
				} `json:"children"`
			} `json:"main"`
		} `json:"playbackActions"`
	}

	detailPagePagination struct {
		Token     string `json:"token"`
		TokenType string `json:"tokenType"`
	}

	detailPageSelf struct {
		GTI  string `json:"gti"`
		Link string `json:"link"`
	}

	detailPageDetail struct {
		ParentTitle   string `json:"parentTitle"`
		Title         string `json:"title"`
		Duration      int32  `json:"duration"`
		SeasonNumber  int32  `json:"seasonNumber"`
		EpisodeNumber int32  `json:"episodeNumber"`
	}
)

func (a *detailPageAction) availableWithPrime() bool {
	for _, p := range a.AcquisitionActions.PrimaryWaysToWatch {
		for _, c := range p.Children {
			if c.SType == "PRIME" {
				return true
			}
		}
	}

	for _, c := range a.AcquisitionActions.MoreWaysToWatch.Children {
		if c.SType == "PRIME" {
			return true
		}
	}

	for _, c := range a.PlaybackActions.Main.Children {
		if c.BenefitID == "freewithads" || c.BenefitID == "FVOD" {
			return true
		}
	}

	return false
}

func (c *amazon) extractDetailPageWidgets(ctx context.Context, domain, id string) (*detailPageWidgets, error) {
	res, err := c.fetchDetailPage(ctx, domain, id, "")
	if err != nil {
		return nil, fmt.Errorf("fetch detail page %q: %w", id, err)
	}

	if !res.Widgets.BuyBox.Action.availableWithPrime() {
		return nil, fmt.Errorf("unavailable with prime %q", id)
	}

	var (
		agg      = res
		page     = res.Widgets.EpisodeList.Actions.Pagination
		nextPage = func(p detailPagePagination) bool { return p.TokenType == "NextPage" }
	)

	for {
		i := slices.IndexFunc(page, nextPage)
		if i == -1 {
			break
		}

		res, err = c.fetchDetailPage(ctx, domain, id, page[i].Token)
		if err != nil {
			return nil, fmt.Errorf("fetch detail page paginated %q: %w", id, err)
		}

		agg.Widgets.EpisodeList.Episodes = append(
			agg.Widgets.EpisodeList.Episodes,
			res.Widgets.EpisodeList.Episodes...,
		)

		page = res.Widgets.EpisodeList.Actions.Pagination
	}

	return &agg.Widgets, nil
}

func (c *amazon) fetchDetailPage(ctx context.Context, domain, id, token string) (*detailPageResponse, error) {
	url, refURL := createURLs(domain, id, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}

	req.Header.Set("Referer", refURL)
	req.Header["x-requested-with"] = []string{"XMLHttpRequest"}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", res.Status)
	}

	var r detailPageResponse
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

func createURLs(domain, id, token string) (string, string) {
	pathPrefix := ""
	if strings.HasPrefix(domain, "amazon") {
		pathPrefix = "/gp/video"
	}

	baseURL := "https://www." + domain + pathPrefix

	refURL := ""
	if strings.HasPrefix(id, "amzn1") {
		refURL = fmt.Sprintf("%s/detail?gti=%s/", baseURL, id)
	} else {
		refURL = fmt.Sprintf("%s/detail/%s/", baseURL, id)
	}

	url := ""
	if token == "" {
		url = fmt.Sprintf(
			"%s/api/getDetailPage?titleID=%s&sections=Atf&sections=Btf&widgets=%s",
			baseURL,
			id,
			urlpkg.QueryEscape(`{"atf":["Self","Header","BuyBox","SeasonSelector"],"btf":["Episodes","Bonus"]}`),
		)
	} else {
		url = fmt.Sprintf(
			"%s/api/getDetailWidgets?titleID=%s&widgets=%s",
			baseURL,
			id,
			urlpkg.QueryEscape(fmt.Sprintf(`[{"widgetType":"EpisodeList","widgetToken":"%s"}]`, token)),
		)
	}

	return url, refURL
}

type movie struct {
	gti      string
	link     string
	title    string
	duration int32
}

func (w *detailPageWidgets) movie() movie {
	return movie{
		gti:      w.Self.GTI,
		link:     w.Self.Link,
		title:    w.Header.Detail.Title,
		duration: w.Header.Detail.Duration,
	}
}

func (c *amazon) sendMovie(ctx context.Context, domain, id string, m movie, results chan<- model.VideoResult) {
	refs, err := c.extractVideoReferences(ctx, domain, m.gti)
	if err != nil {
		results <- model.VideoResult{Err: fmt.Errorf("extract movie reference %q: %w", id, err)}
		return
	}

	results <- model.VideoResult{
		Video: model.Video{
			ID:          m.gti,
			Title:       m.title,
			PlaybackURL: "https://www." + domain + m.link,
			Duration:    m.duration,
		},
		References: refs,
	}
}

type (
	season struct {
		seriesTitle         string
		number              int32
		additionalSeasonIDs []string
		episodes            []episode
	}

	episode struct {
		gti      string
		link     string
		title    string
		duration int32
		number   int32
	}
)

func (w *detailPageWidgets) season() season {
	s := season{
		seriesTitle: w.Header.Detail.ParentTitle,
		number:      w.Header.Detail.SeasonNumber,
	}

	for _, ss := range w.SeasonSelector {
		if !ss.IsSelected {
			s.additionalSeasonIDs = append(s.additionalSeasonIDs, ss.TitleID)
		}
	}

	s.episodes = make([]episode, len(w.EpisodeList.Episodes))
	for i, e := range w.EpisodeList.Episodes {
		s.episodes[i] = episode{
			gti:      e.Self.GTI,
			link:     e.Self.Link,
			title:    e.Detail.Title,
			duration: e.Detail.Duration,
			number:   e.Detail.EpisodeNumber,
		}
	}

	return s
}

func (c *amazon) sendSeries(ctx context.Context, domain, id string, s season, results chan<- model.VideoResult) {
	var wg sync.WaitGroup
	for _, id := range s.additionalSeasonIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()

			w, err := c.extractDetailPageWidgets(ctx, domain, id)
			if err != nil {
				results <- model.VideoResult{Err: err}
				return
			}

			c.sendSeason(ctx, domain, id, w.season(), results)
		}()
	}
	c.sendSeason(ctx, domain, id, s, results)
	wg.Wait()
}

func (c *amazon) sendSeason(ctx context.Context, domain, id string, s season, results chan<- model.VideoResult) {
	var wg sync.WaitGroup
	for _, e := range s.episodes {
		wg.Add(1)
		go func() {
			defer wg.Done()

			refs, err := c.extractVideoReferences(ctx, domain, e.gti)
			if err != nil {
				results <- model.VideoResult{
					Err: fmt.Errorf("extract season reference %q: %w", id, err),
				}
				return
			}

			results <- model.VideoResult{
				Video: model.Video{
					ID:          e.gti,
					Title:       model.OneTitle(s.seriesTitle, e.title, s.number, e.number),
					PlaybackURL: "https://www." + domain + e.link,
					Duration:    e.duration,
				},
				References: refs,
			}
		}()
	}
	wg.Wait()
}

func (c *amazon) extractVideoReferences(ctx context.Context, domain, gti string) ([]model.Reference, error) {
	if gti == "" {
		return nil, errors.New("empty GTI")
	}

	refs := make([]model.Reference, 2)
	g, ctx := errgroup.WithContext(ctx)
	for i, quality := range []string{"sd", "hd"} {
		g.Go(func() error {
			ref, err := c.extractVideoReference(ctx, domain, gti, quality)
			if err != nil {
				return fmt.Errorf("extract video reference %q: %w", gti, err)
			}
			refs[i] = ref
			return nil
		})
	}
	err := g.Wait()

	return refs, err
}

func (c *amazon) extractVideoReference(ctx context.Context, domain, gti, quality string) (model.Reference, error) {
	res, err := c.fetchPlaybackResources(ctx, domain, gti, quality)
	if err != nil {
		return model.Reference{}, fmt.Errorf("fetch playback resources %q: %w", gti, err)
	}
	if res.Error != nil {
		return model.Reference{}, fmt.Errorf("playback resources %q: %w", gti, res.Error)
	}
	if res.ErrorsByResource.PlaybackURLs != nil {
		return model.Reference{}, fmt.Errorf("playback urls %q: %w", gti, res.ErrorsByResource.PlaybackURLs)
	}

	var (
		urlSetID = res.PlaybackURLs.DefaultURLSetID
		manifest = res.PlaybackURLs.URLSets[urlSetID].URLs.Manifest
	)

	url := manifest.URL
	if !strings.Contains(manifest.URL, "encoding=segmentBase") {
		u, err := urlpkg.Parse(manifest.URL)
		if err != nil {
			return model.Reference{}, fmt.Errorf("parse manifest URL: %w", err)
		}

		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += "encoding=segmentBase"

		url = u.String()
	}

	return model.Reference{
		ID:     urlSetID,
		Format: strings.ToLower(manifest.StreamingTechnology),
		URL:    url,
	}, nil
}

type (
	playbackResourcesResponse struct {
		PlaybackURLs playbackURLs `json:"playbackUrls"`

		ErrorsByResource struct {
			PlaybackURLs *playbackResourcesError `json:"PlaybackUrls"`
		} `json:"errorsByResource"`

		Error *playbackResourcesError `json:"error"`
	}

	playbackURLs struct {
		DefaultURLSetID string `json:"defaultUrlSetId"`

		URLSets map[string]struct {
			URLs struct {
				Manifest struct {
					StreamingTechnology string `json:"streamingTechnology"`
					URL                 string `json:"url"`
				} `json:"manifest"`
			} `json:"urls"`
		} `json:"urlSets"`
	}

	playbackResourcesError struct {
		ErrorCode string `json:"errorCode"`
		Message   string `json:"message"`
	}
)

func (e playbackResourcesError) Error() string {
	return e.ErrorCode + ": " + e.Message
}

func (c *amazon) fetchPlaybackResources(ctx context.Context, domain, gti, quality string) (*playbackResourcesResponse, error) {
	const fmtQuery = "?deviceID=%s" +
		"&deviceTypeID=AOAGZA014O5RE" +
		"&firmware=1" +
		"&operatingSystemName=%s" +
		"&asin=%s" +
		"&consumptionType=Streaming" +
		"&desiredResources=PlaybackUrls,CuepointPlaylist" +
		"&resourceUsage=CacheResources" +
		"&videoMaterialType=Feature" +
		"&displayWidth=3840" +
		"&displayHeight=2160" +
		"&vodStreamSupportOverride=Auxiliary" +
		"&deviceStreamingTechnologyOverride=DASH" +
		"&deviceDrmOverride=CENC" +
		"&deviceAdInsertionTypeOverride=SSAI" +
		"&deviceVideoCodecOverride=H264" +
		"&deviceVideoQualityOverride=HD" +
		"&deviceBitrateAdaptationsOverride=CVBR,CBR" +
		"&supportedDRMKeyScheme=DUAL_KEY" +
		"&ssaiSegmentInfoSupport=Base" +
		"&ssaiStitchType=MultiPeriod"

	query := ""
	switch quality {
	case "sd":
		query = fmt.Sprintf(fmtQuery, "479f9d33-f548-4567-89b5-4a36e898b576", "Linux", gti)
	case "hd":
		query = fmt.Sprintf(fmtQuery, "49e8621c-a610-4ba6-9e3a-786b3a2f35cc", "Mac%20OS%20X", gti)
	}

	var (
		switched = switchDomain(domain)
		url      = "https://atv-ps." + switched + ".com/cdp/catalog/GetPlaybackResources" + query
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new: %w", err)
	}

	req.Header.Set("Origin", "https://www."+switched+".com")
	req.Header.Set("Referer", "https://www."+switched+".com/")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", res.Status)
	}

	var r playbackResourcesResponse
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

// Send requests to atv-ps host on alt. domain.
// Hack to avoid 421s.
func switchDomain(domain string) string {
	m := map[string]string{
		"amazon":     "primevideo",
		"primevideo": "amazon",
	}

	return m[strings.SplitN(domain, ".", 2)[0]]
}
