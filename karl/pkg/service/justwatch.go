package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"karl/pkg/config"
)

var _ URLExtractor = (*justWatchURLExtractor)(nil)

type justWatchURLExtractor struct {
	config     *config.AppConfig
	httpClient *http.Client
	packages   []string
	origin     string
}

func NewJustWatchURLExtractor(config *config.AppConfig, httpClient *http.Client, packages []string) *justWatchURLExtractor {
	return &justWatchURLExtractor{
		config:     config,
		httpClient: httpClient,
		packages:   packages,
		origin:     "https://www.justwatch.com",
	}
}

func (c *justWatchURLExtractor) ExtractURLs(ctx context.Context) ([]string, error) {
	var (
		urlSet = make(map[string]struct{})
		mu     sync.Mutex
	)

	g, ctx := errgroup.WithContext(ctx)
	for y := 1950; y <= time.Now().Year(); y++ {
		var (
			minY = y
			maxY = y
		)
		if y == 1950 {
			minY = 1900
		}

		filter := map[string]any{
			"releaseYear": map[string]int{
				"min": minY,
				"max": maxY,
			},
			"excludeIrrelevantTitles": false,
			"packages":                c.packages,
		}

		g.Go(func() error {
			urls, err := c.extractURLs(ctx, filter)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				for _, u := range urls {
					urlSet[u] = struct{}{}
				}
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	urls := make([]string, 0, len(urlSet))
	for url := range urlSet {
		urls = append(urls, url)
	}

	return urls, nil
}

func (c *justWatchURLExtractor) extractURLs(ctx context.Context, filter map[string]any) ([]string, error) {
	const (
		maxReturned   = 1900
		maxIterations = maxReturned / 100
	)

	var (
		urls    []string
		cursor  string
		country = c.config.CountryCode
	)

	for range maxIterations + 1 {
		res, err := c.fetchGraphQLURLs(ctx, filter, country, cursor)
		if err != nil {
			return nil, fmt.Errorf("fetch urls: %w", err)
		}
		if len(res.Errors) > 0 {
			if strings.Contains(res.Errors[0].Message, "locale") {
				country = "US"
				continue
			}
			return nil, res.Errors[0]
		}
		if count := res.Data.PopularTitles.TotalCount; count > maxReturned {
			return nil, fmt.Errorf("too many titles (%d): restrict filter", count)
		}

		urls = append(urls, res.Data.urls()...)

		p := res.Data.PopularTitles.PageInfo
		if !p.HasNextPage {
			return urls, nil
		}
		cursor = p.EndCursor
	}

	return nil, errors.New("too many iterations")
}

func (c *justWatchURLExtractor) fetchGraphQLURLs(ctx context.Context, filter map[string]any, country, cursor string) (*justWatchGraphQLURLResponse, error) {
	const query = "query GetPopularTitles($country: Country! $first: Int! = 100 $after: String " +
		"$popularTitlesFilter: TitleFilter $popularTitlesSortBy: PopularTitlesSorting! = ALPHABETICAL " +
		"$sortRandomSeed: Int! = 0 $watchNowFilter: WatchNowOfferFilter! $offset: Int = 0) " +
		"{ popularTitles(country: $country filter: $popularTitlesFilter first: $first " +
		"sortBy: $popularTitlesSortBy sortRandomSeed: $sortRandomSeed offset: $offset " +
		"after: $after) { edges { node { ...PopularTitleGraphql } } pageInfo { endCursor " +
		"hasNextPage } totalCount } } fragment PopularTitleGraphql on MovieOrShow { watchNowOffer(" +
		"country: $country, platform: WEB, filter: $watchNowFilter) { standardWebURL } }"

	body := map[string]any{
		"operationName": "GetPopularTitles",
		"variables": map[string]any{
			"after":               cursor,
			"offset":              nil,
			"popularTitlesFilter": filter,
			"watchNowFilter": map[string][]string{
				"packages": c.packages,
			},
			"country": country,
		},
		"query": query,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return nil, fmt.Errorf("encode body: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://apis.justwatch.com/graphql",
		&buf,
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

	var r justWatchGraphQLURLResponse
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}

	return &r, nil
}

type (
	justWatchGraphQLURLResponse struct {
		Data   justWatchGraphQLURLData `json:"data"`
		Errors []justWatchGraphQLError `json:"errors"`
	}

	justWatchGraphQLURLData struct {
		PopularTitles struct {
			Edges []struct {
				Node struct {
					WatchNowOffer struct {
						StandardWebURL string `json:"standardWebURL"`
					} `json:"watchNowOffer"`
				} `json:"node"`
			} `json:"edges"`

			PageInfo struct {
				EndCursor   string `json:"endCursor"`
				HasNextPage bool   `json:"hasNextPage"`
			} `json:"pageInfo"`

			TotalCount int `json:"totalCount"`
		} `json:"popularTitles"`
	}

	justWatchGraphQLError struct {
		Message string `json:"message"`

		Extensions struct {
			Code string `json:"code"`
		} `json:"extensions"`
	}
)

func (d *justWatchGraphQLURLData) urls() []string {
	urls := make([]string, 0, len(d.PopularTitles.Edges))
	for _, e := range d.PopularTitles.Edges {
		if url := e.Node.WatchNowOffer.StandardWebURL; url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

func (e justWatchGraphQLError) Error() string {
	return "graphql: " + e.Extensions.Code + ": " + e.Message
}
