package main

import (
	"fmt"
	"html"
	"log"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/daniel-widrick/GraceNoteScraper/guide"
	"github.com/daniel-widrick/GraceNoteScraper/tmdb"
	"github.com/daniel-widrick/GraceNoteScraper/tvlogo"
	"github.com/daniel-widrick/GraceNoteScraper/util"
	"github.com/daniel-widrick/GraceNoteScraper/web"
	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	lang := util.GetEnv("LANGUAGE", "en")
	country := util.GetEnv("COUNTRY", "USA")

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

	client := web.NewClient()

	// Time window: midnight today UTC to +14 days, stepping 6 hours
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	//endTime := midnight.Add(14 * 24 * time.Hour)
	endTime := midnight.Add(time.Hour * 24)

	channelMap := make(map[string]guide.Channel) // channelId -> Channel
	eventMap := make(map[string]bool)            // dedup key -> seen
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
			// Dedup channels by channelId
			if _, exists := channelMap[ch.ChannelID]; !exists {
				channelMap[ch.ChannelID] = guide.ConvertChannel(ch)
			}

			// Process events
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

		// Sleep between requests to avoid problems
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

	tvGuide := guide.TVGuide{
		Channels: channels,
		Programs: programs,
	}

	log.Printf("Rendering XMLTV: %d channels, %d programs", len(channels), len(programs))

	tmpl, err := template.ParseFiles("guide.tmpl")
	if err != nil {
		log.Fatalf("Failed to parse template: %v", err)
	}

	outFile, err := os.Create("xmlguide.xmltv")
	if err != nil {
		log.Fatalf("Failed to create output file: %v", err)
	}
	defer outFile.Close()

	if err := tmpl.Execute(outFile, tvGuide); err != nil {
		log.Fatalf("Failed to execute template: %v", err)
	}

	log.Printf("Wrote guide to xmlguide.xmltv")
}

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
		// Title is XML-escaped in guide.ConvertEvent; unescape for TMDB lookup
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
			// Clear dead zap2it URL — no icon is better than a broken one
			channels[i].IconURL = ""
		}
	}

	log.Printf("TV logos: enriched %d/%d channels", enriched, len(channels))
}
