package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"github.com/daniel-widrick/GraceNoteScraper/util"
)

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_9_3) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/35.0.1916.47 Safari/537.36"

// JSON response structs matching the Gracenote grid API
type GridResponse struct {
	Channels []JSONChannel `json:"channels"`
}

type JSONChannel struct {
	ChannelID     string      `json:"channelId"`
	ChannelNo     string      `json:"channelNo"`
	CallSign      string      `json:"callSign"`
	AffiliateName string      `json:"affiliateName"`
	Thumbnail     string      `json:"thumbnail"`
	Events        []JSONEvent `json:"events"`
}

type JSONEvent struct {
	StartTime string      `json:"startTime"`
	EndTime   string      `json:"endTime"`
	Duration  string      `json:"duration"`
	Thumbnail string      `json:"thumbnail"`
	SeriesID  string      `json:"seriesId"`
	Rating    *string     `json:"rating"`
	Flag      []string    `json:"flag"`
	Tags      []string    `json:"tags"`
	Filter    []string    `json:"filter"`
	Program   JSONProgram `json:"program"`
}

type JSONProgram struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	EpisodeTitle *string `json:"episodeTitle"`
	ShortDesc    *string `json:"shortDesc"`
	Season       *string `json:"season"`
	Episode      *string `json:"episode"`
}

type Preferences struct {
	Country  string
	ZipCode  string
	Headend  string
	LineupId string
	Device   string
	Language string
}

type Client struct {
	*http.Client
	pref Preferences
}

// pageSize is the maximum number of channels the Gracenote grid API
// returns per request.  The scraper paginates using the "startchannel"
// query parameter until a response contains fewer than pageSize channels.
const pageSize = 100

func (c *Client) GetDataByTime(t int64) (*GridResponse, error) {
	log.Printf("headendId=%s lineupId=%s zipCode=%s", c.pref.Headend, c.pref.LineupId, c.pref.ZipCode)

	var allChannels []JSONChannel
	startChannel := 0

	for {
		grid, err := c.fetchGridPage(t, startChannel)
		if err != nil {
			// If we already have some channels, log the error and return
			// what we have rather than failing the entire time slot.
			if len(allChannels) > 0 {
				log.Printf("Warning: pagination stopped at startchannel=%d: %v (returning %d channels so far)", startChannel, err, len(allChannels))
				break
			}
			return nil, err
		}

		allChannels = append(allChannels, grid.Channels...)
		log.Printf("Fetched %d channels (startchannel=%d, total so far=%d)", len(grid.Channels), startChannel, len(allChannels))

		// If we got fewer channels than a full page, we've reached the end.
		if len(grid.Channels) < pageSize {
			break
		}

		startChannel += len(grid.Channels)

		// Safety cap: avoid infinite loops if the API keeps returning
		// full pages for some reason.
		if startChannel > 2000 {
			log.Printf("Warning: channel pagination safety cap reached at %d channels", len(allChannels))
			break
		}

		// Brief pause between pagination requests to be polite.
		time.Sleep(1 * time.Second)
	}

	log.Printf("Total channels for time=%d: %d", t, len(allChannels))
	return &GridResponse{Channels: allChannels}, nil
}

// fetchGridPage retrieves a single page of grid data starting at the
// given channel offset.
func (c *Client) fetchGridPage(t int64, startChannel int) (*GridResponse, error) {
	params := url.Values{
		"aid":            {"orbebb"},
		"lineupId":       {c.pref.LineupId},
		"timespan":       {"6"},
		"headendId":      {c.pref.Headend},
		"country":        {c.pref.Country},
		"device":         {c.pref.Device},
		"postalCode":     {c.pref.ZipCode},
		"isOverride":     {"true"},
		"time":           {fmt.Sprintf("%d", t)},
		"timezone":       {""},
		"pref":           {"16,256"},
		"userId":         {"-"},
		"languagecode":   {c.pref.Language},
		"startchannel":   {fmt.Sprintf("%d", startChannel)},
	}
	gridURL := "https://tvlistings.gracenote.com/api/grid?" + params.Encode()
	log.Printf("Fetching: %s", gridURL)
	req, err := http.NewRequest("GET", gridURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetchGridPage failed to build request: %w", err)
	}
	req.Header.Set("Referer", "https://tvlistings.gracenote.com/grid-affiliates.html?aid=orbebb")
	req.Header.Set("X-Requested-Width", "XMLHttpRequest")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetchGridPage request failed: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unable to read guide body: %w", err)
	}

	var grid GridResponse
	if err := json.Unmarshal(b, &grid); err != nil {
		return nil, fmt.Errorf("unable to parse guide JSON: %w", err)
	}
	return &grid, nil
}

func NewClient() *Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Fatalf("Unable to create cookie storage for http client: %v", err)
		return nil
	}
	return &Client{
		Client: &http.Client{
			Jar:     jar,
			Timeout: 15 * time.Second,
			Transport: &headerTransport{
				rt: http.DefaultTransport,
			},
		},
		pref: Preferences{
			Country:  util.GetEnv("GN_COUNTRY", "USA"),
			ZipCode:  util.GetEnv("GN_ZIPCODE", "13490"),
			Headend:  util.GetEnv("GN_HEADEND", "lineupId"),
			LineupId: util.GetEnv("GN_LINEUP", "USA-lineupId-DEFAULT"),
			Device:   util.GetEnv("GN_DEVICE", "-"),
			Language: util.GetEnv("GN_LANGUAGE", "en-us"),
		},
	}
}

type headerTransport struct {
	rt http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", userAgent)
	return t.rt.RoundTrip(req)
}
