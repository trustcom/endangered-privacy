package config

import (
	"net/http/cookiejar"

	"golang.org/x/time/rate"
)

type AppConfig struct {
	CountryCode    string
	OutDir         string
	NoIndent       bool
	CookieJar      *cookiejar.Jar
	RequestLimiter map[string]*rate.Limiter
	Verbose        bool
}
