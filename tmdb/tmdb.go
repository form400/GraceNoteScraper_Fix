package tmdb

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	baseURL   = "https://api.themoviedb.org"
	imageBase = "https://image.tmdb.org/t/p/w500"
	rateDelay = 250 * time.Millisecond // ~4 req/sec
)

type Client struct {
	http  *http.Client
	cache *Cache
	mu    sync.Mutex // guards rate limiting
	last  time.Time
}

type bearerTransport struct {
	token string
	rt    http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.rt.RoundTrip(req)
}

func NewClient(token, cachePath string) *Client {
	if token == "" {
		return nil
	}
	return &Client{
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &bearerTransport{
				token: token,
				rt:    http.DefaultTransport,
			},
		},
		cache: LoadCache(cachePath),
	}
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	c.cache.Save() //save cache to disk
}

// Lookup checks the cache first, then calls the TMDB search API.
// Returns a CacheEntry with image URL, rating, year, and overview.
func (c *Client) Lookup(title string, isMovie bool) CacheEntry {
	if c == nil {
		return CacheEntry{}
	}

	key := cacheKey(title, isMovie)
	if entry, ok := c.cache.Get(key); ok {
		return entry
	}

	var entry CacheEntry
	if isMovie {
		entry = c.searchMovie(title)
	} else {
		entry = c.searchTV(title)
	}

	c.cache.Set(key, entry)
	return entry
}

func (c *Client) searchTV(title string) CacheEntry {
	return c.search("/3/search/tv", title, false)
}

func (c *Client) searchMovie(title string) CacheEntry {
	return c.search("/3/search/movie", title, true)
}

type searchResponse struct {
	Results []searchResult `json:"results"`
}

type searchResult struct {
	ID               int     `json:"id"`
	PosterPath       *string `json:"poster_path"`
	VoteAverage      float64 `json:"vote_average"`
	Overview         string  `json:"overview"`
	OriginalLanguage string  `json:"original_language"`
	// TV uses first_air_date, movie uses release_date
	FirstAirDate string `json:"first_air_date"`
	ReleaseDate  string `json:"release_date"`
}

func (c *Client) search(path, title string, isMovie bool) CacheEntry {
	c.rateWait()

	u := fmt.Sprintf("%s%s?query=%s", baseURL, path, url.QueryEscape(title))
	resp, err := c.http.Get(u)
	if err != nil {
		log.Printf("tmdb: request failed for %q: %v", title, err)
		return CacheEntry{}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("tmdb: API returned %d for %q", resp.StatusCode, title)
		return CacheEntry{}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("tmdb: failed to read response for %q: %v", title, err)
		return CacheEntry{}
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		log.Printf("tmdb: failed to parse response for %q: %v", title, err)
		return CacheEntry{}
	}

	if len(sr.Results) == 0 {
		return CacheEntry{}
	}

	r := sr.Results[0]
	entry := CacheEntry{
		Rating:       r.VoteAverage,
		Overview:     r.Overview,
		TMDBID:       r.ID,
		OrigLanguage: r.OriginalLanguage,
	}

	if r.PosterPath != nil {
		entry.ImageURL = imageBase + *r.PosterPath
	}

	// Extract year from date string (YYYY-MM-DD)
	dateStr := r.FirstAirDate
	if isMovie {
		dateStr = r.ReleaseDate
	}
	if len(dateStr) >= 4 {
		entry.Year = dateStr[:4]
	}

	return entry
}

// rateWait enforces a minimum delay between API requests.
func (c *Client) rateWait() {
	c.mu.Lock()
	defer c.mu.Unlock()

	since := time.Since(c.last)
	if since < rateDelay {
		time.Sleep(rateDelay - since)
	}
	c.last = time.Now()
}

func cacheKey(title string, isMovie bool) string {
	prefix := "tv"
	if isMovie {
		prefix = "movie"
	}
	return prefix + ":" + title
}
