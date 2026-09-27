package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

type Client struct {
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time

	cacheMu sync.Mutex
	cache   map[string]sellerCacheEntry
}

// sellerCacheEntry holds a short-lived copy of a seller's merged,
// cross-marketplace item list, so repeated requests for the same seller
// don't re-run ~16 eBay calls every time (the Browse API has a daily call
// quota).
type sellerCacheEntry struct {
	items     []json.RawMessage
	expiresAt time.Time
}

const sellerCacheTTL = 60 * time.Second

func NewClient(clientID, clientSecret string) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		cache:        make(map[string]sellerCacheEntry),
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func (c *Client) getToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Now().Before(c.expiresAt) {
		return c.accessToken, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("scope", "https://api.ebay.com/oauth/api_scope")

	req, err := http.NewRequest(http.MethodPost, "https://api.ebay.com/identity/v1/oauth2/token",
		strings.NewReader(form.Encode()))

	if err != nil {
		return "", err
	}

	auth := base64.StdEncoding.EncodeToString([]byte(c.clientID + ":" + c.clientSecret))
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ebay oauth token request failed: %s: %s", resp.Status, body)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}

	c.accessToken = tr.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(tr.ExpiresIn-60) * time.Second)

	return c.accessToken, nil
}

const browseSearchURL = "https://api.ebay.com/buy/browse/v1/item_summary/search"

// supportedMarketplaces lists the eBay marketplaces the Browse API accepts
// (per the API's own error message; EBAY_JP is notably NOT supported, even
// though sellers based in Japan list items under other marketplaces).
// EBAY_US is listed first so that, when an item is de-duplicated because it
// appears under several marketplaces, its US listing (USD pricing) wins.
var supportedMarketplaces = []string{
	"EBAY_US", "EBAY_GB", "EBAY_DE", "EBAY_AU", "EBAY_IT", "EBAY_CA",
	"EBAY_ES", "EBAY_FR", "EBAY_HK", "EBAY_SG", "EBAY_IE", "EBAY_PL",
	"EBAY_NL", "EBAY_AT", "EBAY_CH", "EBAY_BE",
}

const (
	marketplacePageSize    = 200 // eBay Browse API max page size
	maxPagesPerMarketplace = 5   // safety cap: 5 * 200 = 1000 items per marketplace
)

// searchPage is the subset of the eBay Browse API's search response used
// here. Items are kept as json.RawMessage so every field the frontend
// consumes (images, currentBidPrice, etc.) is preserved untouched.
type searchPage struct {
	Total         int               `json:"total"`
	ItemSummaries []json.RawMessage `json:"itemSummaries"`
}

// itemMeta is the subset of item_summary fields needed to de-duplicate and
// sort items across marketplaces.
type itemMeta struct {
	LegacyItemID string `json:"legacyItemId"`
	ItemEndDate  string `json:"itemEndDate"`
}

// searchSellerPage fetches a single page of a seller's items in one marketplace.
func (c *Client) searchSellerPage(seller, sortOrder, marketplace string, limit, offset int) (*searchPage, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("category_ids", "0")
	q.Set("filter", fmt.Sprintf("sellers:{%s},buyingOptions:{AUCTION|FIXED_PRICE}", seller))
	q.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if sortOrder != "" {
		q.Set("sort", sortOrder)
	}

	req, err := http.NewRequest(http.MethodGet, browseSearchURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-EBAY-C-MARKETPLACE-ID", marketplace)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ebay browse search failed (%s): %s: %s", marketplace, resp.Status, body)
	}

	var page searchPage
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("ebay browse search: decoding response (%s): %w", marketplace, err)
	}

	return &page, nil
}

// searchSellerInMarketplace pages through a seller's items in a single
// marketplace, up to maxPagesPerMarketplace pages.
func (c *Client) searchSellerInMarketplace(seller, sortOrder, marketplace string) ([]json.RawMessage, error) {
	var items []json.RawMessage

	offset := 0
	for page := 0; page < maxPagesPerMarketplace; page++ {
		resp, err := c.searchSellerPage(seller, sortOrder, marketplace, marketplacePageSize, offset)
		if err != nil {
			return items, err
		}

		items = append(items, resp.ItemSummaries...)

		offset += marketplacePageSize
		if len(resp.ItemSummaries) < marketplacePageSize || offset >= resp.Total {
			break
		}
	}

	return items, nil
}

// SearchSellerAllMarketplaces fetches a seller's items across every supported
// eBay marketplace, then merges the results: de-duplicating by legacyItemId
// (the same item is often listed under several marketplaces) and sorting by
// end time ascending (soonest first).
//
// A marketplace that errors (rate limit, transient failure, etc.) is skipped
// rather than failing the whole request; an error is only returned if every
// marketplace failed. Results are cached briefly per seller, since this does
// on the order of 16 eBay calls per uncached request.
func (c *Client) SearchSellerAllMarketplaces(seller, sortOrder string) ([]json.RawMessage, error) {
	if items, ok := c.getCachedSeller(seller); ok {
		return items, nil
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		all     []json.RawMessage
		lastErr error
		okCount int
	)

	for _, marketplace := range supportedMarketplaces {
		wg.Add(1)
		go func(marketplace string) {
			defer wg.Done()

			items, err := c.searchSellerInMarketplace(seller, sortOrder, marketplace)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Printf("seller %q: skipping marketplace %s: %v", seller, marketplace, err)
				lastErr = err
			} else {
				okCount++
			}
			all = append(all, items...)
		}(marketplace)
	}
	wg.Wait()

	if okCount == 0 && lastErr != nil {
		return nil, fmt.Errorf("all marketplaces failed, last error: %w", lastErr)
	}

	merged := dedupeAndSortItems(all)
	c.setCachedSeller(seller, merged)
	return merged, nil
}

func (c *Client) getCachedSeller(seller string) ([]json.RawMessage, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	entry, ok := c.cache[seller]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.items, true
}

func (c *Client) setCachedSeller(seller string, items []json.RawMessage) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	c.cache[seller] = sellerCacheEntry{
		items:     items,
		expiresAt: time.Now().Add(sellerCacheTTL),
	}
}

// dedupeAndSortItems removes duplicate items (the same legacyItemId
// appearing under multiple marketplaces — the first occurrence is kept,
// and supportedMarketplaces is ordered with EBAY_US first so USD pricing is
// preferred) and sorts the remaining items by end time ascending
// (endTimeSoonest), items without an end date last.
func dedupeAndSortItems(items []json.RawMessage) []json.RawMessage {
	type entry struct {
		raw  json.RawMessage
		meta itemMeta
	}

	seen := make(map[string]bool, len(items))
	entries := make([]entry, 0, len(items))

	for _, raw := range items {
		var meta itemMeta
		_ = json.Unmarshal(raw, &meta)

		if meta.LegacyItemID != "" {
			if seen[meta.LegacyItemID] {
				continue
			}
			seen[meta.LegacyItemID] = true
		}
		entries = append(entries, entry{raw: raw, meta: meta})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		ei, ej := entries[i].meta.ItemEndDate, entries[j].meta.ItemEndDate
		if ei == "" {
			return false
		}
		if ej == "" {
			return true
		}
		return ei < ej
	})

	result := make([]json.RawMessage, len(entries))
	for i, e := range entries {
		result[i] = e.raw
	}
	return result
}

func ListingHandler(client *Client) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		seller := ctx.DefaultQuery("seller", os.Getenv("DEFAULT_SELLER_USERNAME"))
		sortOrder := ctx.DefaultQuery("sort", "endTimeSoonest")

		// eBay Browse API acepta limit 1-200 y offset >= 0.
		limit, err := strconv.Atoi(ctx.DefaultQuery("limit", "50"))
		if err != nil || limit <= 0 || limit > 200 {
			limit = 50
		}
		offset, err := strconv.Atoi(ctx.DefaultQuery("offset", "0"))
		if err != nil || offset < 0 {
			offset = 0
		}

		// ?marketplace=EBAY_GB fuerza un solo mercado y salta la agregación.
		if marketplace := ctx.Query("marketplace"); marketplace != "" {
			page, err := client.searchSellerPage(seller, sortOrder, marketplace, limit, offset)
			if err != nil {
				ctx.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
				return
			}
			ctx.JSON(http.StatusOK, gin.H{
				"total":         page.Total,
				"itemSummaries": page.ItemSummaries,
			})
			return
		}

		items, err := client.SearchSellerAllMarketplaces(seller, sortOrder)
		if err != nil {
			ctx.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}

		total := len(items)
		start := offset
		if start > total {
			start = total
		}
		end := start + limit
		if end > total {
			end = total
		}

		ctx.JSON(http.StatusOK, gin.H{
			"total":         total,
			"itemSummaries": items[start:end],
		})
	}
}

func main() {
	godotenv.Load()

	client := NewClient(os.Getenv("EBAY_CLIENT_ID"), os.Getenv("EBAY_CLIENT_SECRET"))

	router := gin.Default()
	router.Use(cors.New(cors.Config{
		AllowOrigins: []string{"https://ebay.kaerdos.dev"},
	}))

	router.GET("/listings", ListingHandler(client))

	router.GET("/healtz", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})

	router.GET("/helloworld", func(ctx *gin.Context) {
		ctx.JSON(http.StatusTeapot, gin.H{
			"hello": "teapot",
		})
	})

	router.Run("0.0.0.0:" + os.Getenv("PORT"))
}