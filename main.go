package main

import (
	"log"
	"os"
	"text/template"
	"time"

	"github.com/daniel-widrick/GraceNoteScraper/guide"
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
