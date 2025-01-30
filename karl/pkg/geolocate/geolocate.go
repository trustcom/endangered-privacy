package geolocate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func CountryCode(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipapi.is", nil)
	if err != nil {
		return "", fmt.Errorf("new: %w", err)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do: %w", err)
	}
	defer res.Body.Close()

	var r struct {
		Location struct {
			CountryCode string `json:"country_code"`
		} `json:"location"`
	}
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("decode body: %w", err)
	}

	if r.Location.CountryCode == "" {
		return "", fmt.Errorf("no country code")
	}

	return r.Location.CountryCode, nil
}
