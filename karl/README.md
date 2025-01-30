# Usage

```
$ karl --help

Usage: karl <command> [flags]

Flags:
  -h, --help                   Show context-sensitive help.
      --out-dir=DIRECTORY      Output directory for extracted data. Created if
                               it doesn't exist. Default is current directory
                               ($OUT_DIR)
      --no-indent              Don't indent (beautify) JSON output ($NO_INDENT)
      --country-code=STRING    Two-letter (alpha-2) country code. Recommended
                               to set in alignment with IP location due to
                               potential geo-blocking. If not provided,
                               a geolocation lookup will be done ($COUNTRY_CODE)
      --cookies=HOST=COOKIES,...
                               Cookies to send with each request to host.
                               For example --cookies www.example.com="session=1;
                               token=xyz123",api.io="auth=abc" ($COOKIES)
      --rate-limit=HOST=LIMIT,...
                               Rate limit outbound requests per second for
                               provided hosts. Restrictive defaults are set for
                               known services, to disable (not recommended) set
                               to a negative value ($RATE_LIMIT)
      --verbose                Enable verbose logging (additional error details)
                               ($VERBOSE)

Commands:
  extract-urls <service> [flags]
    Extract all available URLs from service that may link to videos, shows or
    movies

  extract <url> ... [flags]
    Extract and fingerprint service specific URLs to videos, shows or movies.
    Authentication cookies may be required (set via --cookies)

  fingerprint <file|url> [flags]
    Fingerprint file or resource on the web. Must be MPD, M3U8 or fragmented
    MP4 file. If manifest file, base URL is required if not contained within the
    file. If MP4 file or URL, index range may be optionally supplied otherwise
    first 64KB will be read.

Run "karl <command> --help" for more information on a command.
```

