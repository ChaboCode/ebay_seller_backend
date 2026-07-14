package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
}

func NewClient(clientID, clientSecret string) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
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

func (c *Client) SearchBySeller(seller, sort string, limit, offset int) ([]byte, error) {
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
	if sort != "" {
		q.Set("sort", sort)
	}

	req, err := http.NewRequest(http.MethodGet, browseSearchURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-EBAY-C-MARKETPLACE-ID", "EBAY_US")

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
		return nil, fmt.Errorf("ebay browse search failed: %s: %s", resp.Status, body)
	}

	return body, nil
}

func ListingHandler(client *Client) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		seller := ctx.DefaultQuery("seller", os.Getenv("DEFAULT_SELLER_USERNAME"))
		sort := ctx.DefaultQuery("sort", "endTimeSoonest")

		// eBay Browse API acepta limit 1-200 y offset >= 0.
		limit, err := strconv.Atoi(ctx.DefaultQuery("limit", "50"))
		if err != nil || limit <= 0 || limit > 200 {
			limit = 50
		}
		offset, err := strconv.Atoi(ctx.DefaultQuery("offset", "0"))
		if err != nil || offset < 0 {
			offset = 0
		}

		data, err := client.SearchBySeller(seller, sort, limit, offset)
		if err != nil {
			ctx.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}

		ctx.Data(http.StatusOK, "application/json", data)
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