package tvlogo

import (
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	baseRawURL = "https://raw.githubusercontent.com/tv-logo/tv-logos/main/countries"
	rateDelay  = 200 * time.Millisecond // ~5 req/sec
)

type countryInfo struct {
	Dir    string
	Suffix string
}

var countryMap = map[string]countryInfo{
	"USA": {Dir: "united-states", Suffix: "-us"},
	"CAN": {Dir: "canada", Suffix: "-ca"},
	"GBR": {Dir: "united-kingdom", Suffix: "-uk"},
}

// affiliateAliases maps full affiliate names to their common short forms
// when algorithmic normalization wouldn't produce the right slug.
var affiliateAliases = map[string]string{
	"home box office":                              "hbo",
	"national broadcasting company":                "nbc",
	"american broadcasting company":                "abc",
	"cbs television network":                       "cbs",
	"fox entertainment":                            "fox",
	"fox broadcasting":                             "fox",
	"fox broadcasting company":                     "fox",
	"turner network television":                    "tnt",
	"entertainment and sports programming network": "espn",
	"cable news network":                           "cnn",
	"the weather channel":                          "the-weather-channel",
	"comedy central":                               "comedy-central",
	"cartoon network":                              "cartoon-network",
	"animal planet":                                "animal-planet",
	"public broadcasting service":                  "pbs",
	"cable-satellite public affairs network":       "c-span",
}

// noiseWords are stripped from affiliate names during normalization.
var noiseWords = map[string]bool{
	"television":    true,
	"network":       true,
	"channel":       true,
	"broadcasting":  true,
	"company":       true,
	"entertainment": true,
	"corporation":   true,
	"inc":           true,
}

// matches common HD/SD/DT suffixes on callsigns.
var hdSuffixRe = regexp.MustCompile(`(?i)(hd|sd|dt|hd2|hd3|hd4)$`)

// helps split compound callsigns like "ESPNHD" → "espn".
var knownPrefixes = []string{
	"espn", "fox", "hbo", "cnn", "tbs", "tnt", "usa", "amc",
	"bet", "bravo", "mtv", "nick", "syfy", "tlc", "vh1",
	"food", "hgtv", "lifetime", "oxygen", "showtime", "starz",
}

type Client struct {
	http    *http.Client
	cache   *Cache
	mu      sync.Mutex
	last    time.Time
	country countryInfo
}

// creates a tv-logo client for the given Gracenote country code.
// Returns nil if the country is not supported.
func NewClient(gnCountry, cachePath string) *Client {
	ci, ok := countryMap[gnCountry]
	if !ok {
		return nil
	}
	return &Client{
		http: &http.Client{
			Timeout: 10 * time.Second,
		},
		cache:   LoadCache(cachePath),
		country: ci,
	}
}

// saves the cache to disk.
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.cache.Save()
}

// returns a verified logo URL for the channel, or "" if none found.
// Results are cached by channel ID.
func (c *Client) Resolve(channelID, callSign, affiliateName string) string {
	if c == nil {
		return ""
	}

	key := "channelId:" + channelID
	if entry, ok := c.cache.Get(key); ok {
		return entry.LogoURL
	}

	candidates := c.generateCandidates(callSign, affiliateName)

	logoURL := ""
	for _, slug := range candidates {
		u := baseRawURL + "/" + c.country.Dir + "/" + slug + c.country.Suffix + ".png"
		if c.checkURL(u) {
			logoURL = u
			break
		}
	}

	c.cache.Set(key, CacheEntry{LogoURL: logoURL})
	return logoURL
}

// returns an ordered list of slugs to try.
func (c *Client) generateCandidates(callSign, affiliateName string) []string {
	seen := make(map[string]bool)
	var candidates []string

	add := func(slug string) {
		if slug != "" && !seen[slug] {
			seen[slug] = true
			candidates = append(candidates, slug)
		}
	}

	affiliate := strings.ToLower(strings.TrimSpace(affiliateName))
	call := strings.ToLower(strings.TrimSpace(callSign))

	// Check alias table first
	if alias, ok := affiliateAliases[affiliate]; ok {
		add(alias)
	}

	// Normalized affiliate name (strip noise words)
	add(normalizeAffiliate(affiliate))

	// Normalized callsign (strip HD/SD/DT suffixes, try known-prefix split)
	stripped := stripCallSignSuffix(call)
	add(stripped)

	// Try known-prefix extraction for compound callsigns
	for _, prefix := range knownPrefixes {
		if strings.HasPrefix(stripped, prefix) && len(stripped) > len(prefix) {
			add(prefix)
			break
		}
	}

	// Full affiliate name as slug (without stripping noise words)
	add(slugify(affiliate))

	// Raw lowered callsign (suffix-stripped only)
	if stripped != call {
		add(call) // also try the raw form with suffix
	}

	return candidates
}

// strips noise words from the affiliate name and slugifies.
func normalizeAffiliate(name string) string {
	words := strings.Fields(name)
	var kept []string
	for _, w := range words {
		if !noiseWords[w] {
			kept = append(kept, w)
		}
	}
	return slugify(strings.Join(kept, " "))
}

// removes HD/SD/DT suffixes from callsigns.
func stripCallSignSuffix(call string) string {
	return hdSuffixRe.ReplaceAllString(call, "")
}

// converts a name to a URL-safe slug: lowercase, spaces/punctuation to hyphens.
func slugify(s string) string {
	s = strings.ToLower(s)
	// Replace non-alphanumeric with hyphens
	var b strings.Builder
	prev := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prev = false
		} else if !prev {
			b.WriteByte('-')
			prev = true
		}
	}
	result := strings.Trim(b.String(), "-")
	return result
}

// sends an HTTP HEAD request and returns true if the server responds 200.
func (c *Client) checkURL(url string) bool {
	c.rateWait()

	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return false
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// enforces a minimum delay between HTTP requests.
func (c *Client) rateWait() {
	c.mu.Lock()
	defer c.mu.Unlock()

	since := time.Since(c.last)
	if since < rateDelay {
		time.Sleep(rateDelay - since)
	}
	c.last = time.Now()
}
