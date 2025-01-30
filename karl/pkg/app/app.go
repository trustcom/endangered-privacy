package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"karl/pkg/config"
	"karl/pkg/model"
	"karl/pkg/service"
	"karl/pkg/service/amazon"
	"karl/pkg/service/max"
	"karl/pkg/service/svt"
)

type App struct {
	config         *config.AppConfig
	httpClient     *http.Client
	serviceManager *service.Manager
	jsonWriter     *jsonWriter
	outputChan     chan output
	signalChan     chan os.Signal
}

func New(config *config.AppConfig) (*App, error) {
	app := &App{config: config}

	rt := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          400,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	hc := &http.Client{
		Transport: wrapRoundTripper(rt, config),
		Jar:       config.CookieJar,
		Timeout:   3 * time.Minute,
	}
	app.httpClient = hc

	m := service.NewManager(hc, config)
	m.Register(amazon.New)
	m.Register(max.New)
	m.Register(svt.New)
	app.serviceManager = m

	jw, err := newJSONWriter(config)
	if err != nil {
		return nil, err
	}
	app.jsonWriter = jw
	app.outputChan = make(chan output)

	app.signalChan = make(chan os.Signal, 1)
	signal.Notify(app.signalChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	return app, nil
}

func (a *App) OutputHandler(ctx context.Context) {
	for output := range a.outputChan {
		if output.Error != nil {
			if ctx.Err() == nil {
				log.Println(output.Error)
			}
			continue
		}
		if a.config.Verbose {
			if r, ok := output.Result.(model.ExtractResult); ok {
				for _, e := range r.FailedErrors {
					log.Println(e)
				}
			}
		}
		a.jsonWriter.write(output)
	}
}

func (a *App) Close() {
	close(a.outputChan)
}

func (a *App) ShutdownHandler(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	select {
	case <-a.signalChan:
		cancel()
	case <-ctx.Done():
	}
	signal.Stop(a.signalChan)
	a.httpClient.CloseIdleConnections()
}

func (a *App) URLExtract(ctx context.Context, service string) {
	result, err := a.serviceManager.ExtractURLs(ctx, service)
	a.outputChan <- output{Result: result, Prefix: "urls_", Error: err}
}

func (a *App) Extract(ctx context.Context, urls []string, format string) {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(runtime.NumCPU())
	for i, url := range urls {
		g.Go(func() error {
			result, err := a.serviceManager.Extract(ctx, g, url, format)
			a.outputChan <- output{
				Result: result,
				Prefix: "extract_",
				Suffix: fmt.Sprintf("_%05d", i),
				Error:  err,
			}
			return nil
		})
	}
	g.Wait()
}

func (a *App) Fingerprint(ctx context.Context, fileOrURL, baseURL, indexRange string) {
	result, err := a.serviceManager.Fingerprint(ctx, fileOrURL, baseURL, indexRange)
	a.outputChan <- output{Result: result, Prefix: "fingerprint_", Error: err}
}
