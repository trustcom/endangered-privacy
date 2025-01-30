package model

import (
	"fmt"
	"time"
)

type (
	URLExtractResult struct {
		Service string   `json:"service"`
		URLs    []string `json:"urls"`
	}

	ExtractResult struct {
		Service      string  `json:"service"`
		URL          string  `json:"url"`
		Videos       []Video `json:"videos"`
		NumFailed    int     `json:"num_failed"`
		FailedErrors []error `json:"-"`
	}

	FingerprintResult struct {
		URL         string       `json:"url"`
		Variants    *[]Variant   `json:"variant,omitempty"`
		Fingerprint *Fingerprint `json:"fingerprint,omitempty"`
	}

	Video struct {
		ID          string     `json:"id"`
		Title       string     `json:"title"`
		PlaybackURL string     `json:"playback_url"`
		Duration    int32      `json:"duration"`
		ExpiresAt   *time.Time `json:"expires_at"`
		Variants    []Variant  `json:"variants"`
	}

	VideoResult struct {
		Video      Video
		References []Reference
		Err        error
	}

	Reference struct {
		ID      string
		Format  string
		URL     string
		Servers []string
	}

	Variant struct {
		ID        string `json:"-"`
		MimeType  string `json:"mime_type"`
		Codecs    string `json:"codecs"`
		Width     uint32 `json:"width"`
		Height    uint32 `json:"height"`
		Bandwidth uint32 `json:"bandwidth"`

		AddressingMode         string                  `json:"-"`
		IndexedAddressingInfo  *IndexedAddressingInfo  `json:"-"`
		ExplicitAddressingInfo *ExplicitAddressingInfo `json:"-"`

		Fingerprint *Fingerprint `json:"fingerprint"`
	}

	IndexedAddressingInfo struct {
		URL        string
		IndexRange string
	}

	ExplicitAddressingInfo struct {
		TemplateURL      string
		URLs             []string
		Servers          []string
		SegmentDurations []uint32
		Timescale        uint32
	}

	Fingerprint struct {
		SegmentSizes     []uint32 `json:"segment_sizes"`
		SegmentDurations []uint32 `json:"segment_durations"`
		Timescale        uint32   `json:"timescale"`
	}
)

func OneTitle(main, secondary string, season, episode int32) string {
	title := main
	if season > 0 || episode > 0 {
		title += fmt.Sprintf(" S%03dE%03d", season, episode)
		if secondary != "" && secondary != main {
			title += " " + secondary
		}
		return title
	}
	if secondary != "" && secondary != main {
		title += " - " + secondary
	}
	return title
}
