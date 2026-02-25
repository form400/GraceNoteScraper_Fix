package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/daniel-widrick/GraceNoteScraper/guide"
	"github.com/daniel-widrick/GraceNoteScraper/tmdb"
	"github.com/daniel-widrick/GraceNoteScraper/tvlogo"
	"github.com/daniel-widrick/GraceNoteScraper/util"
	"github.com/daniel-widrick/GraceNoteScraper/web"
	"github.com/joho/godotenv"
)

//go:embed guide.tmpl
var guideTmplFS embed.FS

//go:embed index.html
var indexHTML []byte

// ---------- GuideState ----------

// GuideState holds the current guide data, safe for concurrent access.
type GuideState struct {
	mu    sync.RWMutex
	guide *guide.TVGuide
}

func (s *GuideState) Update(g *guide.TVGuide) {
	s.mu.Lock()
	s.guide = g
	s.mu.Unlock()
}

func (s *GuideState) Get() *guide.TVGuide {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.guide
}

// ---------- JSON API types ----------

type APIGuide struct {
	Generated string       `json:"generated"`
	Channels  []APIChannel `json:"channels"`
}

type APIChannel struct {
	ID      string       `json:"id"`
	Number  string       `json:"number"`
	Name    string       `json:"name"`
	LogoURL string       `json:"logoUrl"`
	Programs []APIProgram `json:"programs"`
}

type APIProgram struct {
	Title       string `json:"title"`
	SubTitle    string `json:"subTitle,omitempty"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Category    string `json:"category,omitempty"`
	IsNew       bool   `json:"isNew,omitempty"`
	Rating      string `json:"rating,omitempty"`
	IconURL     string `json:"iconUrl,omitempty"`
	Description string `json:"description,omitempty"`
}

// ---------- Conversion ----------

// guideToJSON converts a TVGuide into the simplified JSON API format.
func guideToJSON(g *guide.TVGuide) APIGuide {
	// Build channel-id -> programs map
	chanProgs := make(map[string][]APIProgram)
	for _, p := range g.Programs {
		cat := ""
		if len(p.Categories) > 0 {
			cat = p.Categories[0].Name
		}
		desc := html.UnescapeString(p.Description)
		if desc == "Unavailable" {
			desc = ""
		}
		iconURL := p.IconSrc
		if iconURL != "" {
			iconURL = "/img?url=" + neturl.QueryEscape(iconURL)
		}
		ap := APIProgram{
			Title:       html.UnescapeString(p.Title),
			SubTitle:    html.UnescapeString(p.SubTitle),
			Start:       xmltvTimeToISO(p.Start),
			End:         xmltvTimeToISO(p.Stop),
			Category:    cat,
			IsNew:       p.New,
			Rating:      p.Rating,
			IconURL:     iconURL,
			Description: desc,
		}
		chanProgs[p.Channel] = append(chanProgs[p.Channel], ap)
	}

	// Sort programs by start time within each channel
	for id := range chanProgs {
		progs := chanProgs[id]
		sort.Slice(progs, func(i, j int) bool {
			return progs[i].Start < progs[j].Start
		})
	}

	var channels []APIChannel
	for _, ch := range g.Channels {
		number := ""
		name := ""
		if len(ch.DisplayNames) >= 3 {
			number = ch.DisplayNames[1].Name // just the number
			name = ch.DisplayNames[2].Name   // just the callsign
		} else if len(ch.DisplayNames) >= 1 {
			name = ch.DisplayNames[0].Name
		}
		logoURL := ch.IconURL
		if logoURL != "" {
			logoURL = "/img?url=" + neturl.QueryEscape(logoURL)
		}
		channels = append(channels, APIChannel{
			ID:       ch.ID,
			Number:   html.UnescapeString(number),
			Name:     html.UnescapeString(name),
			LogoURL:  logoURL,
			Programs: chanProgs[ch.ID],
		})
	}

	// Sort channels by number (numeric sort)
	sort.Slice(channels, func(i, j int) bool {
		return channelNumberLess(channels[i].Number, channels[j].Number)
	})

	return APIGuide{
		Generated: time.Now().UTC().Format(time.RFC3339),
		Channels:  channels,
	}
}

// channelNumberLess compares channel numbers numerically where possible.
func channelNumberLess(a, b string) bool {
	// Try to parse as float for numeric comparison (handles "5.1", "12", etc.)
	var ai, bi float64
	_, errA := fmt.Sscanf(a, "%f", &ai)
	_, errB := fmt.Sscanf(b, "%f", &bi)
	if errA == nil && errB == nil {
		return ai < bi
	}
	return a < b
}

// xmltvTimeToISO converts "20250225200000 +0000" → "2025-02-25T20:00:00Z"
func xmltvTimeToISO(xmltvTime string) string {
	xmltvTime = strings.TrimSpace(xmltvTime)
	// Strip the timezone suffix — we assume +0000 (UTC)
	if idx := strings.Index(xmltvTime, " "); idx >= 0 {
		xmltvTime = xmltvTime[:idx]
	}
	if len(xmltvTime) < 14 {
		return xmltvTime
	}
	return xmltvTime[0:4] + "-" + xmltvTime[4:6] + "-" + xmltvTime[6:8] +
		"T" + xmltvTime[8:10] + ":" + xmltvTime[10:12] + ":" + xmltvTime[12:14] + "Z"
}

// ---------- Scraping ----------

// runScrape performs the full scrape cycle and returns the populated TVGuide.
// It also writes xmlguide.xmltv atomically.
func runScrape(tmdbClient *tmdb.Client, logoClient *tvlogo.Client, lang, country string) (*guide.TVGuide, error) {
	client := web.NewClient()

	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	endTime := midnight.Add(time.Hour * 24)

	channelMap := make(map[string]guide.Channel)
	eventMap := make(map[string]bool)
	var programs []guide.Program

	for t := midnight; t.Before(endTime); t = t.Add(6 * time.Hour) {
		ts := t.Unix()
		log.Printf("Fetching grid for time=%d (%s)", ts, t.Format(time.RFC3339))

		grid, err := client.GetDataByTime(ts)
		if err != nil {
			log.Printf("Error fetching grid at %d: %v", ts, err)
			continue
		}

		for _, ch := range grid.Channels {
			if _, exists := channelMap[ch.ChannelID]; !exists {
				channelMap[ch.ChannelID] = guide.ConvertChannel(ch)
			}

			for _, ev := range ch.Events {
				dedupKey := ch.ChannelID + "|" + ev.StartTime + "|" + ev.EndTime
				if eventMap[dedupKey] {
					continue
				}
				eventMap[dedupKey] = true
				programs = append(programs, guide.ConvertEvent(ev, ch.ChannelID, lang, country))
			}
		}

		log.Printf("Channels so far: %d, Events so far: %d", len(channelMap), len(programs))

		if t.Add(6 * time.Hour).Before(endTime) {
			time.Sleep(5 * time.Second)
		}
	}

	var channels []guide.Channel
	for _, ch := range channelMap {
		channels = append(channels, ch)
	}

	enrichChannelIcons(logoClient, channels)
	enrichProgramThumbnails(tmdbClient, programs)

	tvGuide := &guide.TVGuide{
		Channels: channels,
		Programs: programs,
	}

	log.Printf("Rendering XMLTV: %d channels, %d programs", len(channels), len(programs))

	// Parse embedded template
	tmpl, err := template.ParseFS(guideTmplFS, "guide.tmpl")
	if err != nil {
		return nil, fmt.Errorf("failed to parse template: %w", err)
	}

	// Atomic write: write to temp file, then rename
	tmpFile, err := os.CreateTemp(".", "xmlguide-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmpFile.Name()

	if err := tmpl.Execute(tmpFile, tvGuide); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("failed to execute template: %w", err)
	}
	tmpFile.Close()

	if err := os.Rename(tmpName, "xmlguide.xmltv"); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("failed to rename output file: %w", err)
	}

	log.Printf("Wrote guide to xmlguide.xmltv")
	saveGuideCache(tvGuide)
	return tvGuide, nil
}

// ---------- Guide cache ----------

const guideCachePath = "guide_cache.json"

type guideCache struct {
	SavedAt time.Time      `json:"saved_at"`
	Guide   guide.TVGuide  `json:"guide"`
}

// saveGuideCache persists the TVGuide to a JSON file.
func saveGuideCache(g *guide.TVGuide) {
	data, err := json.Marshal(guideCache{SavedAt: time.Now(), Guide: *g})
	if err != nil {
		log.Printf("guide cache: failed to marshal: %v", err)
		return
	}
	if err := os.WriteFile(guideCachePath, data, 0644); err != nil {
		log.Printf("guide cache: failed to write: %v", err)
		return
	}
	log.Println("Saved guide cache")
}

// loadGuideCache loads the TVGuide from the JSON cache if it's younger than maxAge.
// Returns the guide, its age, and whether it was loaded.
func loadGuideCache(maxAge time.Duration) (*guide.TVGuide, time.Duration, bool) {
	data, err := os.ReadFile(guideCachePath)
	if err != nil {
		return nil, 0, false
	}
	var c guideCache
	if err := json.Unmarshal(data, &c); err != nil {
		log.Printf("guide cache: corrupt, ignoring: %v", err)
		return nil, 0, false
	}
	age := time.Since(c.SavedAt)
	if age >= maxAge {
		return nil, age, false
	}
	return &c.Guide, age, true
}

// ---------- File rotation ----------

// rotateFiles copies the current xmlguide.xmltv to a dated file and prunes old ones.
func rotateFiles() {
	dated := fmt.Sprintf("xmlguide.%s.xmltv", time.Now().UTC().Format("20060102"))

	src, err := os.ReadFile("xmlguide.xmltv")
	if err != nil {
		log.Printf("rotate: failed to read xmlguide.xmltv: %v", err)
		return
	}

	if err := os.WriteFile(dated, src, 0644); err != nil {
		log.Printf("rotate: failed to write %s: %v", dated, err)
		return
	}
	log.Printf("Rotated guide to %s", dated)

	// Prune: keep only the 7 most recent dated files
	matches, _ := filepath.Glob("xmlguide.*.xmltv")
	if len(matches) <= 7 {
		return
	}

	// Sort descending (newest first)
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	for _, old := range matches[7:] {
		log.Printf("Pruning old guide: %s", old)
		os.Remove(old)
	}
}

// ---------- Background scraper ----------

// startScraper runs the scrape cycle on a 24-hour ticker.
// If initialDelay > 0, the first scrape fires after that delay instead of 24h
// (used when we skipped the startup scrape because the file was still fresh).
func startScraper(ctx context.Context, state *GuideState, tmdbClient *tmdb.Client, logoClient *tvlogo.Client, lang, country string, initialDelay time.Duration) {
	if initialDelay <= 0 {
		initialDelay = 24 * time.Hour
	}

	timer := time.NewTimer(initialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Scraper shutting down")
			return
		case <-timer.C:
			log.Println("Starting scheduled scrape cycle")
			g, err := runScrape(tmdbClient, logoClient, lang, country)
			if err != nil {
				log.Printf("Scheduled scrape failed: %v", err)
			} else {
				state.Update(g)
				rotateFiles()
				log.Println("Scheduled scrape complete")
			}
			// All subsequent runs at 24h intervals
			timer.Reset(24 * time.Hour)
		}
	}
}

// ---------- HTTP handlers ----------

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func handleXMLTV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	http.ServeFile(w, r, "xmlguide.xmltv")
}

func handleGuideJSON(state *GuideState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g := state.Get()
		if g == nil {
			http.Error(w, "Guide not available yet", http.StatusServiceUnavailable)
			return
		}

		apiGuide := guideToJSON(g)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.Encode(apiGuide)
	}
}

// ---------- Image proxy ----------

const imageCacheDir = "image_cache"

// imageURLAllowed checks whether a URL is on the proxy allowlist.
func imageURLAllowed(rawURL string) bool {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	if host == "image.tmdb.org" {
		return true
	}
	if host == "raw.githubusercontent.com" && strings.HasPrefix(u.Path, "/tv-logo") {
		return true
	}
	return false
}

func handleImage(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.Error(w, "missing url parameter", http.StatusBadRequest)
		return
	}
	if !imageURLAllowed(rawURL) {
		http.Error(w, "url not allowed", http.StatusForbidden)
		return
	}

	// Cache key
	h := sha256.Sum256([]byte(rawURL))
	key := hex.EncodeToString(h[:])
	datPath := filepath.Join(imageCacheDir, key+".dat")
	typePath := filepath.Join(imageCacheDir, key+".type")

	// Cache hit
	if ct, err := os.ReadFile(typePath); err == nil {
		w.Header().Set("Content-Type", string(ct))
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFile(w, r, datPath)
		return
	}

	// Cache miss — fetch upstream
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream returned "+resp.Status, http.StatusBadGateway)
		return
	}

	// Ensure cache dir
	os.MkdirAll(imageCacheDir, 0755)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "failed reading upstream", http.StatusBadGateway)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Write cache files (best-effort)
	os.WriteFile(datPath, body, 0644)
	os.WriteFile(typePath, []byte(contentType), 0644)

	// Serve
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(body)
}

// ---------- Main ----------

func main() {
	guideOnly := flag.Bool("guide-only", false, "Scrape once and exit (no server)")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	lang := util.GetEnv("LANGUAGE", "en")
	country := util.GetEnv("COUNTRY", "USA")
	port := util.GetEnv("PORT", "8080")

	tmdbToken := util.GetEnv("TMDB_TOKEN", "")
	tmdbClient := tmdb.NewClient(tmdbToken, "tmdb_cache.json")
	if tmdbClient != nil {
		log.Println("TMDB integration enabled")
	} else {
		log.Println("No TMDB token configured, skipping image enrichment")
	}
	defer tmdbClient.Close()

	logoClient := tvlogo.NewClient(country, "tvlogo_cache.json")
	if logoClient != nil {
		log.Println("TV logo enrichment enabled")
	} else {
		log.Printf("TV logo enrichment not available for country %s", country)
	}
	defer logoClient.Close()

	// --guide-only: always scrape, write output, exit
	if *guideOnly {
		log.Println("Starting scrape (guide-only mode)...")
		if _, err := runScrape(tmdbClient, logoClient, lang, country); err != nil {
			log.Fatalf("Scrape failed: %v", err)
		}
		log.Println("--guide-only: done")
		return
	}

	// Server mode: try loading cached guide data to skip a slow scrape
	var g *guide.TVGuide
	var nextScrapeIn time.Duration
	if cached, age, ok := loadGuideCache(4 * time.Hour); ok {
		log.Printf("Loaded guide from cache (%s old), skipping scrape", age.Round(time.Second))
		g = cached
		// Schedule next scrape for when the cache turns 24h old
		nextScrapeIn = 24*time.Hour - age
		if nextScrapeIn < time.Hour {
			nextScrapeIn = time.Hour
		}
	} else {
		log.Println("Starting initial scrape...")
		var err error
		g, err = runScrape(tmdbClient, logoClient, lang, country)
		if err != nil {
			log.Fatalf("Initial scrape failed: %v", err)
		}
		rotateFiles()
		nextScrapeIn = 24 * time.Hour
	}

	state := &GuideState{}
	state.Update(g)

	// Signal context for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start background scraper
	log.Printf("Next scrape in %s", nextScrapeIn.Round(time.Minute))
	go startScraper(ctx, state, tmdbClient, logoClient, lang, country, nextScrapeIn)

	// HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/xmlguide.xmltv", handleXMLTV)
	mux.HandleFunc("/api/guide.json", handleGuideJSON(state))
	mux.HandleFunc("/img", handleImage)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	// Start server in background
	go func() {
		log.Printf("HTTP server listening on :%s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-ctx.Done()
	log.Println("Shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	log.Println("Goodbye")
}

// ---------- Enrichment helpers ----------

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// replaces broken Gracenote thumbnail URLs with TMDB
// poster images, star ratings, dates, and descriptions.
func enrichProgramThumbnails(client *tmdb.Client, programs []guide.Program) {
	if client == nil {
		return
	}

	// Phase 1: collect unique {title, isMovie} pairs
	type titleKey struct {
		title   string
		isMovie bool
	}
	seen := make(map[titleKey]bool)
	var unique []titleKey

	for _, p := range programs {
		title := strings.ToLower(html.UnescapeString(p.Title))
		isMovie := false
		for _, cat := range p.Categories {
			if cat.Name == "movie" {
				isMovie = true
				break
			}
		}
		k := titleKey{title: title, isMovie: isMovie}
		if !seen[k] {
			seen[k] = true
			unique = append(unique, k)
		}
	}

	log.Printf("TMDB: looking up %d unique titles", len(unique))

	// Phase 2: lookup each unique title
	results := make(map[titleKey]tmdb.CacheEntry)
	for _, k := range unique {
		results[k] = client.Lookup(k.title, k.isMovie)
	}

	// Phase 3: apply results back to programs
	enriched := 0
	for i := range programs {
		title := strings.ToLower(html.UnescapeString(programs[i].Title))
		isMovie := false
		for _, cat := range programs[i].Categories {
			if cat.Name == "movie" {
				isMovie = true
				break
			}
		}
		entry := results[titleKey{title: title, isMovie: isMovie}]
		if entry.TMDBID == 0 && entry.ImageURL == "" && entry.Rating == 0 {
			continue
		}
		enriched++
		if entry.ImageURL != "" {
			programs[i].IconSrc = entry.ImageURL
			programs[i].Images = []guide.Image{{
				URL:    entry.ImageURL,
				Type:   "poster",
				Size:   "3",
				Orient: "P",
				System: "tmdb",
			}}
		}
		if entry.Rating > 0 {
			programs[i].StarRating = fmt.Sprintf("%.1f/10", entry.Rating)
		}
		if entry.Year != "" {
			programs[i].Date = entry.Year
		}
		if entry.Overview != "" && programs[i].Description == "Unavailable" {
			programs[i].Description = xmlEscape(entry.Overview)
		}
		if entry.OrigLanguage != "" {
			programs[i].OrigLanguage = entry.OrigLanguage
		}
		if entry.TMDBID != 0 {
			tmdbEpNum := fmt.Sprintf("%d", entry.TMDBID)
			if !isMovie {
				tmdbEpNum = fmt.Sprintf("series/%d", entry.TMDBID)
			}
			programs[i].EpisodeNumbers = append(programs[i].EpisodeNumbers, guide.EpisodeNumber{
				System:        "themoviedb.org",
				EpisodeNumber: tmdbEpNum,
			})
		}
	}

	log.Printf("TMDB: enriched %d/%d programs", enriched, len(programs))
}

// resolves channel logos from the tv-logo/tv-logos repo,
// replacing dead Gracenote icon URLs with verified GitHub-hosted PNGs.
func enrichChannelIcons(client *tvlogo.Client, channels []guide.Channel) {
	if client == nil {
		return
	}

	log.Printf("TV logos: resolving icons for %d channels", len(channels))

	enriched := 0
	for i := range channels {
		logoURL := client.Resolve(channels[i].ID, channels[i].CallSign, channels[i].Affiliate)
		if logoURL != "" {
			channels[i].IconURL = logoURL
			enriched++
		} else {
			channels[i].IconURL = ""
		}
	}

	log.Printf("TV logos: enriched %d/%d channels", enriched, len(channels))
}
