package classifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
)

const defaultEndpoint = "https://graphql.anilist.co"

// Cache is the subset of store.Store used to short-circuit AniList lookups.
// Pass nil to New/NewWithCache to skip caching (tests do this).
type Cache interface {
	GetCachedClassification(titleNorm string) (model.ContentType, bool, error)
	CacheClassification(titleNorm string, t model.ContentType) error
}

// MetricsSink is the subset of the metrics.Registry interface the classifier uses.
// A nil value is safe: all calls are guarded with a nil check.
type MetricsSink interface {
	IncAniListLookup(result string)
}

type Classifier struct {
	endpoint string
	http     *http.Client
	cache    Cache        // may be nil
	Metrics  MetricsSink  // optional; nil disables all metric calls
}

// New creates a Classifier that queries the given endpoint (use "" for the
// live AniList endpoint). Tests pass an httptest.Server URL here.
func New(endpoint string) *Classifier {
	return NewWithCache(endpoint, nil)
}

// NewWithCache creates a Classifier that checks the cache before hitting
// the network and writes through after a successful lookup.
func NewWithCache(endpoint string, cache Cache) *Classifier {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &Classifier{endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}, cache: cache}
}

const query = `query ($s: String) { Media(search: $s, type: MANGA) { countryOfOrigin } }`

type anilistResp struct {
	Data struct {
		Media *struct {
			CountryOfOrigin string `json:"countryOfOrigin"`
		} `json:"Media"`
	} `json:"data"`
}

// Classify returns the content type for a title, or TypeUnknown if AniList
// has no match or the country is unmapped. Results are read from and written
// to the cache (when a Cache is configured) to respect AniList's rate limit.
func (c *Classifier) Classify(title string) (model.ContentType, error) {
	// Cache read — skip network if we already know the answer.
	if c.cache != nil {
		if ct, ok, err := c.cache.GetCachedClassification(title); err == nil && ok {
			if c.Metrics != nil {
				c.Metrics.IncAniListLookup("cached")
			}
			return ct, nil
		}
	}

	body, _ := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]string{"s": title},
	})
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, fmt.Errorf("anilist rate limited")
	}
	if resp.StatusCode >= 400 {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, fmt.Errorf("anilist status %d", resp.StatusCode)
	}
	var out anilistResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, err
	}
	if out.Data.Media == nil {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("miss")
		}
		return model.TypeUnknown, nil
	}
	ct := model.CountryToType(out.Data.Media.CountryOfOrigin)

	// Cache write-through (best-effort; ignore error so caller still gets result).
	if c.cache != nil {
		_ = c.cache.CacheClassification(title, ct)
	}

	if c.Metrics != nil {
		c.Metrics.IncAniListLookup("success")
	}
	return ct, nil
}
