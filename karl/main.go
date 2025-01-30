package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"
	"golang.org/x/time/rate"
	"karl/pkg/app"
	"karl/pkg/config"
	"karl/pkg/geolocate"

	"github.com/alecthomas/kong"
	"github.com/joho/godotenv"
)

var CLI struct {
	ExtractURLs struct {
		Service string `arg:"" name:"service" help:"Service to extract URLs from"`
	} `cmd:"" name:"extract-urls" help:"Extract all available URLs from service that may link to videos, shows or movies"`

	Extract struct {
		URLs   []string `arg:"" name:"url" help:"URLs to extract. URLs don't have to be from the same service."`
		Format string   `enum:"dash,hls,both" default:"dash" placeholder:"FORMAT" help:"Limit fingerprinting to specific ABR format: \"dash\", \"hls\" or \"both\". Default is \"dash\""`
	} `cmd:"" help:"Extract and fingerprint service specific URLs to videos, shows or movies. Authentication cookies may be required (set via --cookies)"`

	Fingerprint struct {
		FileOrURL  string `arg:"" name:"file|url" help:"File or URL to fingerprint"`
		BaseURL    string `help:"Base URL for manifest files, required if not contained within manifest"`
		IndexRange string `help:"Byte-range of the index segment in the fragmented MP4 file. If not supplied will read first 64KB"`
	} `cmd:"" help:"Fingerprint file or resource on the web. Must be MPD, M3U8 or fragmented MP4 file. If manifest file, base URL is required if not contained within the file. If MP4 file or URL, index range may be optionally supplied otherwise first 64KB will be read."`

	OutDir      string            `env:"OUT_DIR" default:"." placeholder:"DIRECTORY" help:"Output directory for extracted data. Created if it doesn't exist. Default is current directory"`
	NoIndent    bool              `env:"NO_INDENT" help:"Don't indent (beautify) JSON output"`
	CountryCode string            `env:"COUNTRY_CODE" help:"Two-letter (alpha-2) country code. Recommended to set in alignment with IP location due to potential geo-blocking. If not provided, a geolocation lookup will be done"`
	Cookies     map[string]string `env:"COOKIES" mapsep:"," placeholder:"HOST=COOKIES,..." help:"Cookies to send with each request to host. For example --cookies www.example.com=\"session=1; token=xyz123\",api.io=\"auth=abc\""`
	RateLimit   map[string]int    `env:"RATE_LIMIT" mapsep:"," placeholder:"HOST=LIMIT,..." help:"Rate limit outbound requests per second for provided hosts. Restrictive defaults are set for known services, to disable (not recommended) set to a negative value"`
	Verbose     bool              `env:"VERBOSE" help:"Enable verbose logging (additional error details)"`
}

func main() {
	godotenv.Load()
	kongCtx := kong.Parse(&CLI)
	config := &config.AppConfig{
		OutDir:   CLI.OutDir,
		NoIndent: CLI.NoIndent,
		Verbose:  CLI.Verbose,
	}

	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	for host, cookieStr := range CLI.Cookies {
		cookies, err := http.ParseCookie(cookieStr)
		if err != nil {
			kongCtx.FatalIfErrorf(err)
		}
		jar.SetCookies(&url.URL{Scheme: "https", Host: host}, cookies)
	}
	config.CookieJar = jar

	requestLimiter := map[string]*rate.Limiter{
		"www.amazon.com":                  rate.NewLimiter(rate.Limit(2), 2),
		"www.primevideo.com":              rate.NewLimiter(rate.Limit(2), 2),
		"default.any-any.prd.api.max.com": rate.NewLimiter(rate.Limit(10), 10),
		"video.svt.se":                    rate.NewLimiter(rate.Limit(10), 10),
	}
	for host, rateLimit := range CLI.RateLimit {
		if rateLimit < 0 {
			delete(requestLimiter, host)
			continue
		}
		requestLimiter[host] = rate.NewLimiter(rate.Limit(rateLimit), rateLimit)
	}
	config.RequestLimiter = requestLimiter

	app, err := app.New(config)
	if err != nil {
		kongCtx.FatalIfErrorf(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		app.OutputHandler(ctx)
		cancel()
	}()
	go func() {
		defer wg.Done()
		app.ShutdownHandler(ctx, cancel)
	}()
	defer func() {
		app.Close()
		wg.Wait()
	}()

	countryCode := strings.ToUpper(CLI.CountryCode)
	if countryCode != "" && len(countryCode) != 2 {
		kongCtx.Errorf("invalid two-letter country code: %q", countryCode)
		return
	}
	if countryCode == "" {
		countryCode, err = geolocate.CountryCode(ctx)
		if err != nil {
			kongCtx.Errorf("no country code set and geolocate failed: %v", err)
			return
		}
	}
	config.CountryCode = countryCode

	switch kongCtx.Command() {
	case "extract-urls <service>":
		app.URLExtract(ctx, CLI.ExtractURLs.Service)
	case "extract <url>":
		app.Extract(ctx, CLI.Extract.URLs, CLI.Extract.Format)
	case "fingerprint <file|url>":
		app.Fingerprint(ctx, CLI.Fingerprint.FileOrURL, CLI.Fingerprint.BaseURL, CLI.Fingerprint.IndexRange)
	default:
		kongCtx.Errorf("unknown command")
	}
}
